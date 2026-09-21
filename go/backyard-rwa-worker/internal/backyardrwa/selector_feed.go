package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const kaminoStatsEndpoint = "https://api.kamino.finance/reserves/batch/stats"

// The verification view already owns the confirmed-state join. Join its exact
// event for the two v2 fields not projected by that view; never use latest raw
// events or API stats as a substitute for confirmed reserve evidence.
const selectorReserveSQL = `SELECT jsonb_build_object(
 'reserve',v.reserve,'market',v.market,'mint',v.liquidity_mint,
 'observedAt',v.verified_at,'slot',v.verified_slot,'hash',v.account_data_hash,
 'commitment',v.verification_commitment,'supplyApy',v.supply_apy,'borrowApy',v.borrow_apy,
 'debtSupplyRaw',v.total_supply_amount,'debtBorrowRaw',v.borrowed_amount,
 'status',v.reserve_status,'emergency',v.emergency_mode,'curve',v.borrow_rate_curve,
 'hostBps',s.snapshot->'host_fixed_interest_rate_bps',
 'schema',s.snapshot->'observation_schema_version')
 FROM kamino.latest_verified_reserve_updates v
 JOIN kamino.reserve_updates s ON s.event_id=v.event_id AND s.reserve=v.reserve AND s.account_data_hash=v.account_data_hash
 WHERE v.reserve=ANY($1::text[])`

type verifiedEconomicReserve struct {
	Reserve    string             `json:"reserve"`
	Market     string             `json:"market"`
	Mint       string             `json:"mint"`
	ObservedAt time.Time          `json:"observedAt"`
	Slot       int64              `json:"slot"`
	Hash       string             `json:"hash"`
	Commitment string             `json:"commitment"`
	SupplyAPY  *float64           `json:"supplyApy"`
	BorrowAPY  *float64           `json:"borrowApy"`
	SupplyRaw  *float64           `json:"debtSupplyRaw"`
	BorrowRaw  *float64           `json:"debtBorrowRaw"`
	Status     *int               `json:"status"`
	Emergency  *bool              `json:"emergency"`
	Curve      []BorrowCurvePoint `json:"curve"`
	HostBPS    *float64           `json:"hostBps"`
	Schema     *int               `json:"schema"`
}

type nativeYield struct {
	APY        float64   `json:"apy"`
	ObservedAt time.Time `json:"observedAt"`
	EvidenceID string    `json:"evidenceId"`
}

// The official API returns a dictionary keyed by reserve and string decimals.
// Only verified native yield is consumed here; headline Multiply APY, aggregate
// capacity, and unproven rewards never authorize entry.
func parseNativeYields(body []byte, routes []RuntimeRoute, now time.Time, maxAge time.Duration) (map[string]nativeYield, error) {
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
	out := map[string]nativeYield{}
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
		out[route.Lane] = nativeYield{rate, y.ObservedAt, sha256Bytes(body)}
	}
	return out, nil
}

func fetchNativeYields(ctx context.Context, client *http.Client, endpoint string, routes []RuntimeRoute, now time.Time, age time.Duration) (map[string]nativeYield, error) {
	if client == nil {
		return nil, fmt.Errorf("kamino_stats_client_unavailable")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("kamino_stats_endpoint_invalid")
	}
	reserves := make([]string, 0, len(routes))
	for _, r := range routes {
		reserves = append(reserves, r.Kamino.CollateralReserve)
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
	return parseNativeYields(body, routes, now, age)
}

func readVerifiedEconomics(ctx context.Context, pool *pgxpool.Pool, routes []RuntimeRoute) (map[string]verifiedEconomicReserve, error) {
	ids := make([]string, 0, len(routes)*2)
	for _, r := range routes {
		ids = append(ids, r.Kamino.CollateralReserve, r.Kamino.DebtReserve)
	}
	rows, err := pool.Query(ctx, selectorReserveSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("verified_reserve_feed_unavailable")
	}
	defer rows.Close()
	out := map[string]verifiedEconomicReserve{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("verified_reserve_feed_invalid")
		}
		var r verifiedEconomicReserve
		if json.Unmarshal(raw, &r) != nil {
			return nil, fmt.Errorf("verified_reserve_feed_invalid")
		}
		if _, duplicate := out[r.Reserve]; duplicate {
			return nil, fmt.Errorf("verified_reserve_feed_duplicate")
		}
		out[r.Reserve] = r
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("verified_reserve_feed_interrupted")
	}
	return out, nil
}

// combineEconomics is the public installed wrapper: lane acceptance stays the
// installed selectorLane set, byte-identical to the pre-candidate behavior.
func combineEconomics(routes []RuntimeRoute, reserves map[string]verifiedEconomicReserve, yields map[string]nativeYield, now time.Time, p SelectorPolicy) []LaneEconomics {
	return combineEconomicsWithLane(routes, reserves, yields, now, p, selectorLane)
}

// combineEconomicsWithLane is the shared combine core with the lane authority
// parameterized. Every identity, freshness, rate, debt and curve check is
// unchanged; only WHICH lanes may contribute evidence moves, and the caller
// owns that authority (the feed supplies its manifest-scoped set).
func combineEconomicsWithLane(routes []RuntimeRoute, reserves map[string]verifiedEconomicReserve, yields map[string]nativeYield, now time.Time, p SelectorPolicy, laneAllowed func(string) bool) []LaneEconomics {
	out := make([]LaneEconomics, 0, len(routes))
	valid := func(r verifiedEconomicReserve, reserve, mint, market string) bool {
		return r.Reserve == reserve && r.Mint == mint && r.Market == market && r.Commitment == "confirmed" && r.Slot > 0 && sha256Pattern.MatchString(r.Hash) && r.Schema != nil && *r.Schema == 2 && r.Status != nil && r.Emergency != nil && freshAt(now, r.ObservedAt, p.MarketMaxAge)
	}
	for _, route := range routes {
		c, d := reserves[route.Kamino.CollateralReserve], reserves[route.Kamino.DebtReserve]
		y, ok := yields[route.Lane]
		if !ok || !valid(c, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, route.Kamino.Market) || !valid(d, route.Kamino.DebtReserve, route.Kamino.DebtMint, route.Kamino.Market) || c.SupplyAPY == nil || d.BorrowAPY == nil || d.SupplyRaw == nil || d.BorrowRaw == nil || d.HostBPS == nil {
			continue
		}
		// loyal-kamino-codec emits reserve supply and borrowed amounts in raw
		// token units. Every installed debt lane is USDC, and the candidate
		// AUTO debt lane is PYUSD — both 6-decimal assets, so no conversion.
		e := LaneEconomics{Lane: route.Lane, EvidenceID: c.Hash + ":" + d.Hash + ":" + y.EvidenceID, ObservedAt: c.ObservedAt, NativeObservedAt: y.ObservedAt, NativeAPY: y.APY, SupplyAPY: *c.SupplyAPY, CurrentBorrowAPY: *d.BorrowAPY, BorrowCurve: d.Curve, HostBorrowBPS: *d.HostBPS, DebtSupplyRaw: *d.SupplyRaw, DebtBorrowRaw: *d.BorrowRaw}
		if d.ObservedAt.Before(e.ObservedAt) {
			e.ObservedAt = d.ObservedAt
		}
		// Feed capacity is deliberately not pair admission. The action-time adapter
		// supplies exact execution-size capacity after policies, caps and swaps pass.
		if *c.Status != 0 || *d.Status != 0 || *c.Emergency || *d.Emergency {
			e.EntryBlockedReason = "reserve_inactive_or_emergency"
		}
		if e.validateWithLane(now, p, laneAllowed) == nil {
			out = append(out, e)
		}
	}
	return out
}

// EconomicFeed refreshes off the execution path. Snapshot never waits on HTTP
// or SQL, and failed refreshes do not make stale entries fresh. Safety and
// withdrawal decisions can run while enrichment is down.
type EconomicFeed struct {
	pool   *pgxpool.Pool
	client *http.Client
	routes []RuntimeRoute
	// candidateLane is non-empty ONLY when the reviewed manifest carried a
	// valid existing AutoPolicy binding at construction. It is the feed's
	// manifest authority: the candidate route's evidence passes the shared
	// content checks with THIS lane allowed — never a global allowlist change.
	candidateLane string
	mu            sync.RWMutex
	latest        []LaneEconomics
	failure       string
}

// NewEconomicFeed is the preserved embedded constructor: the route inventory
// is exactly the installed selector lanes. The read-only session params and
// timeouts are shared with the manifest-scoped constructor below.
func NewEconomicFeed(ctx context.Context, databaseURL string) (*EconomicFeed, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("TIMESCALEDB_URL is required for economic observations")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid economic database configuration")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	// Enforce read-only sessions even if the injected credential can write.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("economic database unavailable")
	}
	routes := make([]RuntimeRoute, 0, len(selectorLanes))
	for _, lane := range selectorLanes {
		r, _ := runtimeRoute(lane)
		routes = append(routes, r)
	}
	return &EconomicFeed{pool: pool, client: &http.Client{Timeout: 5 * time.Second}, routes: routes}, nil
}

// NewEconomicFeedOnManifest scopes the route inventory to the SAME reviewed
// manifest the worker's selector evaluates against, so the feed can never
// observe a lane the selector's manifest does not bind and cannot miss one it
// does. Installed lanes keep the embedded constructor's exact inventory; the
// candidate AUTO route joins ONLY when the manifest carries a valid existing
// RuntimeBindings.AutoPolicy binding — never from runtime embedded JSON
// activation, catalog defaults, or a malformed binding. Absence and a
// malformed binding both leave the inventory identical to the embedded one,
// and the candidate route is appended last, after every installed lane.
func NewEconomicFeedOnManifest(ctx context.Context, databaseURL string, manifest RouteManifest) (*EconomicFeed, error) {
	feed, err := NewEconomicFeed(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if _, err := manifest.autoPolicyBinding(); err != nil {
		return feed, nil
	}
	feed.routes = append(feed.routes, autoAUTOPYUSD)
	feed.candidateLane = autoAUTOPYUSD.Lane
	return feed, nil
}
func (f *EconomicFeed) Close() { f.pool.Close() }
func (f *EconomicFeed) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	now := time.Now().UTC()
	p := DefaultSelectorPolicy()
	reserves, err := readVerifiedEconomics(ctx, f.pool, f.routes)
	var yields map[string]nativeYield
	if err == nil {
		yields, err = fetchNativeYields(ctx, f.client, kaminoStatsEndpoint, f.routes, now, p.NativeMaxAge)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.failure = err.Error()
		return err
	}
	// Manifest authority stays with the feed: installed lanes pass through the
	// selectorLane set, and the candidate lane is allowed ONLY because this
	// feed's own constructor proved the manifest binding. The candidate lane
	// is never written into a global allowlist.
	laneAllowed := func(lane string) bool {
		return selectorLane(lane) || (f.candidateLane != "" && lane == f.candidateLane)
	}
	f.latest = combineEconomicsWithLane(f.routes, reserves, yields, now, p, laneAllowed)
	f.failure = ""
	if len(f.latest) != len(f.routes) {
		f.failure = "economic_evidence_incomplete"
	}
	return nil
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
