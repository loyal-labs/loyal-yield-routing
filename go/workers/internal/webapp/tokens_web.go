package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	webTokenCacheControl   = "public, max-age=60, s-maxage=300, stale-while-revalidate=3600"
	webMarketsCacheTTL     = 15 * time.Minute
	webMarketsCacheControl = "public, max-age=60, s-maxage=900, stale-while-revalidate=3600"
	webMarketsPerCall      = 30
	tickerCacheControl     = "public, max-age=60, s-maxage=300, stale-while-revalidate=86400, stale-if-error=86400"
	tickerFresh            = 5 * time.Minute
	tickerStale            = 24 * time.Hour
	tickerTimeout          = 5 * time.Second
	jupiterSearchURL       = "https://lite-api.jup.ag/tokens/v2/search"
	loyalTokenMint         = "LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta"
)

var webMintPattern = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)

type poolOHLCV struct {
	timeframe string
	limit     int
}

var webChartDays = map[string]poolOHLCV{
	"1":   {"hour", 24},
	"7":   {"hour", 168},
	"30":  {"day", 30},
	"365": {"day", 365},
	"max": {"day", 1000},
}

type ttlCache[V any] struct {
	mu        sync.Mutex
	items     map[string]ttlItem[V]
	lastPrune time.Time
}

type ttlItem[V any] struct {
	expires time.Time
	value   V
}

func (c *ttlCache[V]) get(key string, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok || !item.expires.After(now) {
		delete(c.items, key)
		var zero V
		return zero, false
	}
	return item.value, true
}

func (c *ttlCache[V]) put(key string, value V, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]ttlItem[V]{}
	}
	if now.Sub(c.lastPrune) >= time.Minute {
		c.lastPrune = now
		for k, item := range c.items {
			if !item.expires.After(now) {
				delete(c.items, k)
			}
		}
	}
	c.items[key] = ttlItem[V]{expires: now.Add(ttl), value: value}
}

func sharedFlight[V any](ctx context.Context, g *singleflight.Group, key string, work func(context.Context) (V, error)) (V, error) {
	result := g.DoChan(key, func() (any, error) {
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*coinGeckoTimeout)
		defer cancel()
		return work(shared)
	})
	select {
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			var zero V
			return zero, r.Err
		}
		return r.Val.(V), nil
	}
}

type webTokens struct {
	gecko      *tokenDetails
	jupiterURL string
	jupiter    *http.Client
	flights    singleflight.Group

	details    ttlCache[*MobileTokenDetailResponse]
	charts     ttlCache[[]MobileTokenChartPoint]
	markets    ttlCache[*webTokenMarket]
	priceCache ttlCache[webTokenPrice]

	tickerMu    sync.Mutex
	tickerEntry *tickerEntry
}

type tickerEntry struct {
	fresh, stale time.Time
	value        *loyalTicker
}

func newWebTokens(gecko *tokenDetails) *webTokens {
	return &webTokens{gecko: gecko, jupiterURL: jupiterSearchURL, jupiter: &http.Client{Timeout: tickerTimeout}}
}

type webTokenInput struct {
	Mint      string `path:"mint" doc:"Solana token mint, base58"`
	ChartDays string `query:"chartDays" doc:"1, 7, 30, 365 or max; returns only the chart for that range"`
}

type webTokenMarketInput struct {
	Mint string `path:"mint" doc:"Solana token mint, base58"`
}

type webTokenMarketsInput struct {
	Mints string `query:"mints" doc:"Comma separated mints"`
}

type webTokenOutput[T any] struct {
	CacheControl string `header:"Cache-Control"`
	Body         T
}

type webTokenChart struct {
	Chart []MobileTokenChartPoint `json:"chart" nullable:"false"`
}

type webTokenMarket struct {
	Mint   string                 `json:"mint"`
	Token  MobileTokenDetailToken `json:"token"`
	Market struct {
		PriceUSD *float64 `json:"priceUsd"`
	} `json:"market"`
}

type webTokenPrice struct {
	Mint                  string   `json:"mint"`
	PriceUSD              *float64 `json:"priceUsd"`
	PriceChange24hPercent *float64 `json:"priceChange24hPercent"`
}

type webTokenPrices struct {
	Markets []webTokenPrice `json:"markets" nullable:"false"`
}

type loyalTicker struct {
	Symbol            string   `json:"symbol"`
	Icon              string   `json:"icon"`
	UsdPrice          float64  `json:"usdPrice"`
	PriceChange24hPct *float64 `json:"priceChange24hPct"`
}

func webFailure(status int, message, cacheControl string) error {
	header := http.Header{}
	if cacheControl != "" {
		header.Set("Cache-Control", cacheControl)
	}
	return huma.ErrorWithHeaders(&contentError{Message: message, status: status}, header)
}

func webMint(raw string) (string, bool) {
	mint := strings.TrimSpace(raw)
	return mint, webMintPattern.MatchString(mint)
}

func registerWebReads[I, O any](api huma.API, id, path string, handler func(context.Context, *I) (*O, error)) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		huma.Register(api, huma.Operation{OperationID: method + "-" + id, Method: method, Path: path, Hidden: true}, handler)
	}
}

func registerWebTokens(api huma.API, tokens *webTokens) {
	registerWebReads(api, "web-token", "/api/tokens/{mint}", func(ctx context.Context, in *webTokenInput) (*webTokenOutput[any], error) {
		mint, ok := webMint(in.Mint)
		if !ok {
			return nil, webFailure(http.StatusBadRequest, "Invalid token mint", webTokenCacheControl)
		}
		if in.ChartDays != "" {
			if _, ok := webChartDays[in.ChartDays]; !ok {
				return nil, webFailure(http.StatusBadRequest, "Invalid chartDays", webTokenCacheControl)
			}
			chart, err := tokens.chart(ctx, mint, in.ChartDays)
			if err != nil {
				return nil, webFailure(http.StatusInternalServerError, "Failed to fetch token detail", "")
			}
			return &webTokenOutput[any]{CacheControl: webTokenCacheControl, Body: webTokenChart{Chart: chart}}, nil
		}
		detail, err := tokens.detail(ctx, mint)
		if err != nil {
			return nil, webFailure(http.StatusInternalServerError, "Failed to fetch token detail", "")
		}
		return &webTokenOutput[any]{CacheControl: webTokenCacheControl, Body: detail}, nil
	})
	registerWebReads(api, "web-token-market", "/api/tokens/{mint}/market", func(ctx context.Context, in *webTokenMarketInput) (*webTokenOutput[*webTokenMarket], error) {
		mint, ok := webMint(in.Mint)
		if !ok {
			return nil, webFailure(http.StatusBadRequest, "Invalid token mint", webTokenCacheControl)
		}
		market, err := tokens.market(ctx, mint)
		if err != nil {
			return nil, webFailure(http.StatusInternalServerError, "Failed to fetch token market", "")
		}
		return &webTokenOutput[*webTokenMarket]{CacheControl: webTokenCacheControl, Body: market}, nil
	})
	registerWebReads(api, "web-token-markets", "/api/tokens/markets", func(ctx context.Context, in *webTokenMarketsInput) (*webTokenOutput[webTokenPrices], error) {
		if in.Mints == "" {
			return nil, webFailure(http.StatusBadRequest, "mints query parameter is required", "")
		}
		var mints []string
		for _, value := range strings.Split(in.Mints, ",") {
			if value = strings.TrimSpace(value); value != "" {
				mints = append(mints, value)
			}
		}
		prices, err := tokens.prices(ctx, mints)
		if err != nil {
			return nil, webFailure(http.StatusInternalServerError, "Failed to fetch token markets", "")
		}
		return &webTokenOutput[webTokenPrices]{CacheControl: webMarketsCacheControl, Body: webTokenPrices{Markets: prices}}, nil
	})
	registerWebReads(api, "web-token-loyal-ticker", "/api/tokens/loyal-ticker", func(ctx context.Context, _ *struct{}) (*webTokenOutput[*loyalTicker], error) {
		ticker, err := tokens.ticker(ctx)
		if err != nil {
			return nil, webFailure(http.StatusServiceUnavailable, "LOYAL ticker is temporarily unavailable", "no-store")
		}
		return &webTokenOutput[*loyalTicker]{CacheControl: tickerCacheControl, Body: ticker}, nil
	})
}

func (w *webTokens) now() time.Time { return w.gecko.now() }

func (w *webTokens) detail(ctx context.Context, mint string) (*MobileTokenDetailResponse, error) {
	if cached, ok := w.details.get(mint, w.now()); ok {
		return cached, nil
	}
	return sharedFlight(ctx, &w.flights, "detail:"+mint, func(ctx context.Context) (*MobileTokenDetailResponse, error) {
		var token *geckoToken
		var info *geckoInfo
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); token = w.gecko.token(ctx, mint) }()
		go func() { defer wg.Done(); info = w.gecko.info(ctx, mint) }()
		wg.Wait()

		var chart []MobileTokenChartPoint
		var volumeOverride *float64
		if token != nil {
			chart, volumeOverride = w.defaultChart(ctx, token)
		}
		detail := buildTokenDetail(mint, token, info, chart, volumeOverride)
		if len(detail.Chart) >= 2 {
			w.details.put(mint, detail, w.now(), tokenDetailCacheTTL)
		}
		return detail, nil
	})
}

func (w *webTokens) defaultChart(ctx context.Context, token *geckoToken) ([]MobileTokenChartPoint, *float64) {
	if token.coinID != nil && *token.coinID != "" {
		if points, volume := w.coinChart(ctx, *token.coinID, "1"); len(points) > 0 {
			return points, volume
		}
	}
	if len(token.topPoolIDs) > 0 {
		if points := w.poolChart(ctx, token.topPoolIDs[0], "hour", 0); len(points) > 0 {
			return points, nil
		}
	}
	return nil, nil
}

func (w *webTokens) chart(ctx context.Context, mint, days string) ([]MobileTokenChartPoint, error) {
	key := mint + ":" + days
	if cached, ok := w.charts.get(key, w.now()); ok {
		return cached, nil
	}
	return sharedFlight(ctx, &w.flights, "chart:"+key, func(ctx context.Context) ([]MobileTokenChartPoint, error) {
		token := w.gecko.token(ctx, mint)
		if token == nil {
			return []MobileTokenChartPoint{}, nil
		}
		var chart []MobileTokenChartPoint
		if token.coinID != nil && *token.coinID != "" {
			chart, _ = w.coinChart(ctx, *token.coinID, days)
		}
		if len(chart) == 0 && len(token.topPoolIDs) > 0 {
			window := webChartDays[days]
			chart = w.poolChart(ctx, token.topPoolIDs[0], window.timeframe, window.limit)
		}
		if len(chart) == 0 {
			return []MobileTokenChartPoint{}, nil
		}
		w.charts.put(key, chart, w.now(), tokenDetailCacheTTL)
		return chart, nil
	})
}

func (w *webTokens) coinChart(ctx context.Context, coinID, days string) ([]MobileTokenChartPoint, *float64) {
	var resp struct {
		Prices       [][2]float64 `json:"prices"`
		TotalVolumes [][2]float64 `json:"total_volumes"`
	}
	if err := w.gecko.get(ctx, "/coins/"+url.PathEscape(coinID)+"/market_chart?vs_currency=usd&days="+days, &resp); err != nil {
		return nil, nil
	}
	points := make([]MobileTokenChartPoint, len(resp.Prices))
	for i, p := range resp.Prices {
		points[i] = MobileTokenChartPoint{Timestamp: p[0], PriceUSD: p[1]}
	}
	var volume *float64
	if n := len(resp.TotalVolumes); n > 0 {
		volume = &resp.TotalVolumes[n-1][1]
	}
	return points, volume
}

func (w *webTokens) poolChart(ctx context.Context, poolID, timeframe string, limit int) []MobileTokenChartPoint {
	var resp struct {
		Data struct {
			Attributes struct {
				OhlcvList [][]float64 `json:"ohlcv_list"`
			} `json:"attributes"`
		} `json:"data"`
	}
	path := "/onchain/networks/solana/pools/" + url.PathEscape(poolID) + "/ohlcv/" + timeframe
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := w.gecko.get(ctx, path, &resp); err != nil {
		return nil
	}
	var points []MobileTokenChartPoint
	for _, candle := range resp.Data.Attributes.OhlcvList {
		if len(candle) >= 5 {
			points = append(points, MobileTokenChartPoint{Timestamp: candle[0], PriceUSD: candle[4]})
		}
	}
	return points
}

func (w *webTokens) market(ctx context.Context, mint string) (*webTokenMarket, error) {
	if cached, ok := w.markets.get(mint, w.now()); ok {
		return cached, nil
	}
	return sharedFlight(ctx, &w.flights, "market:"+mint, func(ctx context.Context) (*webTokenMarket, error) {
		token := w.gecko.token(ctx, mint)
		if token == nil {
			token = &geckoToken{}
		}
		market := &webTokenMarket{
			Mint:  mint,
			Token: MobileTokenDetailToken{Decimals: token.decimals, LogoURL: token.imageURL, Name: token.name, Symbol: token.symbol},
		}
		market.Market.PriceUSD = token.priceUSD
		w.markets.put(mint, market, w.now(), tokenDetailCacheTTL)
		return market, nil
	})
}

func (w *webTokens) prices(ctx context.Context, requested []string) ([]webTokenPrice, error) {
	if len(requested) == 0 {
		return []webTokenPrice{}, nil
	}
	mints := slices.Clone(requested)
	slices.Sort(mints)
	mints = slices.Compact(mints)

	now := w.now()
	fresh := []webTokenPrice{}
	var stale []string
	for _, mint := range mints {
		if price, ok := w.priceCache.get(mint, now); ok {
			fresh = append(fresh, price)
		} else {
			stale = append(stale, mint)
		}
	}
	if len(stale) == 0 {
		return fresh, nil
	}

	var batches [][]string
	for start := 0; start < len(stale); start += webMarketsPerCall {
		batches = append(batches, stale[start:min(start+webMarketsPerCall, len(stale))])
	}
	fetched := make([][]webTokenPrice, len(batches))
	var group errgroup.Group
	for i, batch := range batches {
		group.Go(func() error {
			prices, err := sharedFlight(ctx, &w.flights, "prices:"+strings.Join(batch, ","), func(ctx context.Context) ([]webTokenPrice, error) {
				return w.priceBatch(ctx, batch)
			})
			fetched[i] = prices
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	result := fresh
	expiresFrom := w.now()
	for _, prices := range fetched {
		for _, price := range prices {
			w.priceCache.put(price.Mint, price, expiresFrom, webMarketsCacheTTL)
		}
		result = append(result, prices...)
	}
	return result, nil
}

func (w *webTokens) priceBatch(ctx context.Context, mints []string) ([]webTokenPrice, error) {
	var resp struct {
		Data struct {
			Attributes struct {
				TokenPrices map[string]looseNumber `json:"token_prices"`
				Changes     map[string]looseNumber `json:"h24_price_change_percentage"`
			} `json:"attributes"`
		} `json:"data"`
	}
	escaped := make([]string, len(mints))
	for i, mint := range mints {
		escaped[i] = url.PathEscape(mint)
	}
	path := "/onchain/simple/networks/solana/token_price/" + strings.Join(escaped, ",") + "?include_24hr_price_change=true"
	if err := w.gecko.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	prices := make([]webTokenPrice, len(mints))
	for i, mint := range mints {
		a := resp.Data.Attributes
		prices[i] = webTokenPrice{Mint: mint, PriceUSD: a.TokenPrices[mint].value, PriceChange24hPercent: a.Changes[mint].value}
	}
	return prices, nil
}

func (w *webTokens) ticker(ctx context.Context) (*loyalTicker, error) {
	now := w.now()
	w.tickerMu.Lock()
	entry := w.tickerEntry
	w.tickerMu.Unlock()
	if entry != nil && entry.fresh.After(now) {
		return entry.value, nil
	}
	var stale *loyalTicker
	if entry != nil && entry.stale.After(now) {
		stale = entry.value
	}
	return sharedFlight(ctx, &w.flights, "ticker", func(ctx context.Context) (*loyalTicker, error) {
		value, err := w.fetchTicker(ctx)
		if err != nil {
			if stale != nil {
				return stale, nil
			}
			return nil, err
		}
		saved := w.now()
		w.tickerMu.Lock()
		w.tickerEntry = &tickerEntry{fresh: saved.Add(tickerFresh), stale: saved.Add(tickerStale), value: value}
		w.tickerMu.Unlock()
		return value, nil
	})
}

func (w *webTokens) fetchTicker(ctx context.Context) (*loyalTicker, error) {
	ctx, cancel := context.WithTimeout(ctx, tickerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.jupiterURL+"?"+url.Values{"query": {loyalTokenMint}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := w.jupiter.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("jupiter token request failed with %d", resp.StatusCode)
	}
	var payload []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, coinGeckoMaxBody)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("jupiter returned invalid LOYAL ticker data: %w", err)
	}
	for _, raw := range payload {
		if ticker := parseJupiterToken(raw); ticker != nil {
			return ticker, nil
		}
	}
	return nil, fmt.Errorf("jupiter returned invalid LOYAL ticker data")
}

func parseJupiterToken(raw json.RawMessage) *loyalTicker {
	var token struct {
		ID       any `json:"id"`
		Address  any `json:"address"`
		Symbol   any `json:"symbol"`
		Icon     any `json:"icon"`
		UsdPrice any `json:"usdPrice"`
		Stats24h struct {
			PriceChange any `json:"priceChange"`
		} `json:"stats24h"`
	}
	if json.Unmarshal(raw, &token) != nil {
		return nil
	}
	mint := token.ID
	if mint == nil {
		mint = token.Address
	}
	icon, _ := token.Icon.(string)
	price := tickerUsdPrice(token.UsdPrice)
	if mint != loyalTokenMint || token.Symbol != "LOYAL" || !safeIconURL(icon) || price == nil {
		return nil
	}
	ticker := &loyalTicker{Symbol: "LOYAL", Icon: icon, UsdPrice: *price}
	if change, ok := token.Stats24h.PriceChange.(float64); ok && !math.IsNaN(change) && !math.IsInf(change, 0) {
		ticker.PriceChange24hPct = &change
	}
	return ticker
}

func tickerUsdPrice(value any) *float64 {
	var price float64
	switch v := value.(type) {
	case float64:
		price = v
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		price = parsed
	default:
		return nil
	}
	if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
		return nil
	}
	return &price
}

func safeIconURL(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "/") {
		return !strings.HasPrefix(value, "//")
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}
