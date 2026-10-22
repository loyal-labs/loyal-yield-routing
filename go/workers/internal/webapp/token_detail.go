package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"golang.org/x/sync/singleflight"
)

const (
	coinGeckoBaseURL    = "https://pro-api.coingecko.com/api/v3"
	tokenDetailCacheTTL = 5 * time.Minute
	coinGeckoTimeout    = 10 * time.Second
	coinGeckoMaxBody    = 8 << 20
)

type chartWindow struct {
	days        string
	granularity string
	limit       int
}

var chartWindows = map[string]chartWindow{
	"1d": {"1", "hour", 24},
	"1w": {"7", "hour", 168},
	"1m": {"30", "day", 30},
	"1y": {"365", "day", 365},
}

type tokenDetails struct {
	apiKey  string
	baseURL string
	client  *http.Client
	now     func() time.Time
	flights singleflight.Group
	mu      sync.Mutex
	cache   map[string]cachedDetail
}

type cachedDetail struct {
	expires time.Time
	value   *MobileTokenDetailResponse
}

func newTokenDetails(apiKey string) *tokenDetails {
	return &tokenDetails{
		apiKey:  apiKey,
		baseURL: coinGeckoBaseURL,
		client:  &http.Client{Timeout: coinGeckoTimeout},
		now:     time.Now,
		cache:   map[string]cachedDetail{},
	}
}

type tokenDetailInput struct {
	Mint      string `path:"mint" doc:"Solana token mint, base58"`
	Timeframe string `query:"timeframe" doc:"1d, 1w, 1m or 1y; anything else reads as 1d"`
}

type tokenDetailOutput struct {
	ContentHeaders
	Body *MobileTokenDetailResponse
}

func registerTokens(api huma.API, details *tokenDetails) {
	registerReads(api, "mobile-token-detail", "/api/mobile/tokens/{mint}", "Token detail from CoinGecko", func(ctx context.Context, in *tokenDetailInput) (*tokenDetailOutput, error) {
		if key, err := solana.PublicKeyFromBase58(in.Mint); err != nil || key.String() != in.Mint {
			return nil, contentFailure(http.StatusBadRequest, "Invalid mint")
		}
		timeframe := in.Timeframe
		if _, ok := chartWindows[timeframe]; !ok {
			timeframe = "1d"
		}
		body, err := details.detail(ctx, in.Mint, timeframe)
		if err != nil {
			return nil, contentFailure(http.StatusInternalServerError, "Failed to fetch token detail")
		}
		return &tokenDetailOutput{ContentHeaders: newContentHeaders(), Body: body}, nil
	})
}

type MobileTokenDetailResponse struct {
	Mint   string                  `json:"mint"`
	Token  MobileTokenDetailToken  `json:"token"`
	Links  MobileTokenDetailLinks  `json:"links"`
	Market MobileTokenDetailMarket `json:"market"`
	Info   MobileTokenDetailInfo   `json:"info"`
	Chart  []MobileTokenChartPoint `json:"chart" nullable:"false"`
}

type MobileTokenDetailToken struct {
	Decimals *float64 `json:"decimals"`
	LogoURL  *string  `json:"logoUrl"`
	Name     *string  `json:"name"`
	Symbol   *string  `json:"symbol"`
}

type MobileTokenDetailLinks struct {
	Website  *string `json:"website"`
	Twitter  *string `json:"twitter"`
	Explorer *string `json:"explorer"`
	Discord  *string `json:"discord"`
	Telegram *string `json:"telegram"`
}

type MobileTokenDetailMarket struct {
	FdvUSD                *float64 `json:"fdvUsd"`
	HolderCount           *float64 `json:"holderCount"`
	LiquidityUSD          *float64 `json:"liquidityUsd"`
	MarketCapUSD          *float64 `json:"marketCapUsd"`
	PriceChange24hPercent *float64 `json:"priceChange24hPercent"`
	PriceUSD              *float64 `json:"priceUsd"`
	UpdatedAt             *string  `json:"updatedAt"`
	Volume24hUSD          *float64 `json:"volume24hUsd"`
}

type MobileTokenDetailInfo struct {
	Description        *string                    `json:"description"`
	GtScore            *float64                   `json:"gtScore"`
	GtVerified         bool                       `json:"gtVerified"`
	MintAuthority      *string                    `json:"mintAuthority"`
	FreezeAuthority    *string                    `json:"freezeAuthority"`
	HolderDistribution NullableHolderDistribution `json:"holderDistribution"`
}

type NullableHolderDistribution struct {
	Value *MobileTokenHolderDistribution
}

func (n NullableHolderDistribution) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

func (NullableHolderDistribution) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{
		r.Schema(reflect.TypeFor[MobileTokenHolderDistribution](), true, "MobileTokenHolderDistribution"),
		{Type: "null"},
	}}
}

type MobileTokenHolderDistribution struct {
	Top10 string `json:"top10"`
	Rest  string `json:"rest"`
}

type MobileTokenChartPoint struct {
	Timestamp float64 `json:"timestamp"`
	PriceUSD  float64 `json:"priceUsd"`
}

func (d *tokenDetails) detail(ctx context.Context, mint, timeframe string) (*MobileTokenDetailResponse, error) {
	key := mint + ":" + timeframe
	if cached := d.cached(key); cached != nil {
		return cached, nil
	}
	result := d.flights.DoChan(key, func() (any, error) {
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*coinGeckoTimeout)
		defer cancel()
		return d.build(shared, key, mint, timeframe), nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*MobileTokenDetailResponse), nil
	}
}

func (d *tokenDetails) cached(key string) *MobileTokenDetailResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.cache[key]
	if !ok || !entry.expires.After(d.now()) {
		delete(d.cache, key)
		return nil
	}
	return entry.value
}

func (d *tokenDetails) store(key string, detail *MobileTokenDetailResponse) {
	change := detail.Market.PriceChange24hPercent
	if (change == nil || math.IsNaN(*change) || math.IsInf(*change, 0)) && len(detail.Chart) < 2 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for k, entry := range d.cache {
		if !entry.expires.After(now) {
			delete(d.cache, k)
		}
	}
	d.cache[key] = cachedDetail{expires: now.Add(tokenDetailCacheTTL), value: detail}
}

func (d *tokenDetails) build(ctx context.Context, key, mint, timeframe string) *MobileTokenDetailResponse {
	var token *geckoToken
	var info *geckoInfo
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); token = d.token(ctx, mint) }()
	go func() { defer wg.Done(); info = d.info(ctx, mint) }()
	wg.Wait()

	var chart []MobileTokenChartPoint
	var volumeOverride *float64
	if token != nil {
		chart, volumeOverride = d.chart(ctx, token, timeframe)
	}
	detail := buildTokenDetail(mint, token, info, chart, volumeOverride)
	d.store(key, detail)
	return detail
}

type geckoToken struct {
	name, symbol, imageURL, coinID *string
	decimals                       *float64
	priceUSD, marketCapUSD, fdvUSD *float64
	volumeUSD24h, totalReserveUSD  *float64
	topPoolIDs                     []string
}

type geckoInfo struct {
	websites                       []string
	twitterHandle, discordURL      *string
	telegramHandle, description    *string
	gtScore, holderCount           *float64
	gtVerified                     bool
	holderDistribution             *MobileTokenHolderDistribution
	mintAuthority, freezeAuthority *string
}

type lenient[T any] struct{ value *T }

func (l *lenient[T]) UnmarshalJSON(raw []byte) error {
	l.value = nil
	var v T
	if json.Unmarshal(raw, &v) == nil && string(raw) != "null" {
		l.value = &v
	}
	return nil
}

type looseNumber struct{ value *float64 }

func (n *looseNumber) UnmarshalJSON(raw []byte) error {
	n.value = nil
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	n.value = &f
	return nil
}

func (d *tokenDetails) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-cg-pro-api-key", d.apiKey)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("coingecko status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, coinGeckoMaxBody)).Decode(out)
}

func (d *tokenDetails) token(ctx context.Context, mint string) *geckoToken {
	var resp struct {
		Data struct {
			Attributes struct {
				Name         *string     `json:"name"`
				Symbol       *string     `json:"symbol"`
				Decimals     *float64    `json:"decimals"`
				ImageURL     *string     `json:"image_url"`
				PriceUSD     looseNumber `json:"price_usd"`
				MarketCapUSD looseNumber `json:"market_cap_usd"`
				FdvUSD       looseNumber `json:"fdv_usd"`
				VolumeUSD    struct {
					H24 looseNumber `json:"h24"`
				} `json:"volume_usd"`
				TotalReserve  looseNumber `json:"total_reserve_in_usd"`
				CoingeckoCoin *string     `json:"coingecko_coin_id"`
			} `json:"attributes"`
			Relationships struct {
				TopPools struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"top_pools"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := d.get(ctx, "/onchain/networks/solana/tokens/"+url.PathEscape(mint), &resp); err != nil {
		return nil
	}
	a := resp.Data.Attributes
	token := &geckoToken{
		name: a.Name, symbol: a.Symbol, imageURL: a.ImageURL, decimals: a.Decimals, coinID: a.CoingeckoCoin,
		priceUSD: a.PriceUSD.value, marketCapUSD: a.MarketCapUSD.value, fdvUSD: a.FdvUSD.value,
		volumeUSD24h: a.VolumeUSD.H24.value, totalReserveUSD: a.TotalReserve.value,
	}
	for _, pool := range resp.Data.Relationships.TopPools.Data {
		token.topPoolIDs = append(token.topPoolIDs, strings.TrimPrefix(pool.ID, "solana_"))
	}
	return token
}

func (d *tokenDetails) info(ctx context.Context, mint string) *geckoInfo {
	var resp struct {
		Data struct {
			Attributes struct {
				Websites       lenient[[]string] `json:"websites"`
				TwitterHandle  lenient[string]   `json:"twitter_handle"`
				DiscordURL     lenient[string]   `json:"discord_url"`
				TelegramHandle lenient[string]   `json:"telegram_handle"`
				Description    lenient[string]   `json:"description"`
				GtScore        looseNumber       `json:"gt_score"`
				GtVerified     lenient[bool]     `json:"gt_verified"`
				Holders        lenient[struct {
					Count        looseNumber `json:"count"`
					Distribution lenient[struct {
						Top10 lenient[string] `json:"top_10"`
						Rest  lenient[string] `json:"rest"`
					}] `json:"distribution_percentage"`
				}] `json:"holders"`
				MintAuthority   lenient[string] `json:"mint_authority"`
				FreezeAuthority lenient[string] `json:"freeze_authority"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := d.get(ctx, "/onchain/networks/solana/tokens/"+url.PathEscape(mint)+"/info", &resp); err != nil {
		return nil
	}
	a := resp.Data.Attributes
	info := &geckoInfo{
		twitterHandle: a.TwitterHandle.value, discordURL: a.DiscordURL.value, telegramHandle: a.TelegramHandle.value,
		gtScore: a.GtScore.value, mintAuthority: a.MintAuthority.value, freezeAuthority: a.FreezeAuthority.value,
	}
	if a.Websites.value != nil {
		info.websites = *a.Websites.value
	}
	info.gtVerified = a.GtVerified.value != nil && *a.GtVerified.value
	if a.Description.value != nil {
		if trimmed := strings.TrimSpace(*a.Description.value); trimmed != "" {
			info.description = &trimmed
		}
	}
	if holders := a.Holders.value; holders != nil {
		info.holderCount = holders.Count.value
		if dist := holders.Distribution.value; dist != nil && dist.Top10.value != nil && dist.Rest.value != nil {
			info.holderDistribution = &MobileTokenHolderDistribution{Top10: *dist.Top10.value, Rest: *dist.Rest.value}
		}
	}
	return info
}

func (d *tokenDetails) chart(ctx context.Context, token *geckoToken, timeframe string) ([]MobileTokenChartPoint, *float64) {
	window := chartWindows[timeframe]
	if token.coinID != nil && *token.coinID != "" {
		var resp struct {
			Prices       [][2]float64 `json:"prices"`
			TotalVolumes [][2]float64 `json:"total_volumes"`
		}
		path := "/coins/" + url.PathEscape(*token.coinID) + "/market_chart?vs_currency=usd&days=" + window.days
		if err := d.get(ctx, path, &resp); err == nil && len(resp.Prices) > 0 {
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
	}
	if len(token.topPoolIDs) > 0 {
		var resp struct {
			Data struct {
				Attributes struct {
					OhlcvList [][]float64 `json:"ohlcv_list"`
				} `json:"attributes"`
			} `json:"data"`
		}
		path := fmt.Sprintf("/onchain/networks/solana/pools/%s/ohlcv/%s?limit=%d", url.PathEscape(token.topPoolIDs[0]), window.granularity, window.limit)
		if err := d.get(ctx, path, &resp); err == nil {
			var points []MobileTokenChartPoint
			for _, candle := range resp.Data.Attributes.OhlcvList {
				if len(candle) >= 5 {
					points = append(points, MobileTokenChartPoint{Timestamp: candle[0], PriceUSD: candle[4]})
				}
			}
			if len(points) > 0 {
				sort.SliceStable(points, func(i, j int) bool { return points[i].Timestamp < points[j].Timestamp })
				return points, nil
			}
		}
	}
	return nil, nil
}

func buildTokenDetail(mint string, token *geckoToken, info *geckoInfo, chart []MobileTokenChartPoint, volumeOverride *float64) *MobileTokenDetailResponse {
	if token == nil {
		token = &geckoToken{}
	}
	if info == nil {
		info = &geckoInfo{}
	}
	if chart == nil {
		chart = []MobileTokenChartPoint{}
	}
	explorer := "https://solscan.io/token/" + mint
	detail := &MobileTokenDetailResponse{
		Mint:  mint,
		Token: MobileTokenDetailToken{Decimals: token.decimals, LogoURL: token.imageURL, Name: token.name, Symbol: token.symbol},
		Links: MobileTokenDetailLinks{Explorer: &explorer, Discord: info.discordURL},
		Market: MobileTokenDetailMarket{
			FdvUSD: token.fdvUSD, HolderCount: info.holderCount, LiquidityUSD: token.totalReserveUSD, MarketCapUSD: token.marketCapUSD,
			PriceChange24hPercent: priceChange24h(chart), PriceUSD: token.priceUSD, Volume24hUSD: token.volumeUSD24h,
		},
		Info: MobileTokenDetailInfo{
			Description: info.description, GtScore: info.gtScore, GtVerified: info.gtVerified,
			MintAuthority: info.mintAuthority, FreezeAuthority: info.freezeAuthority, HolderDistribution: NullableHolderDistribution{info.holderDistribution},
		},
		Chart: chart,
	}
	if volumeOverride != nil {
		detail.Market.Volume24hUSD = volumeOverride
	}
	if len(info.websites) > 0 {
		detail.Links.Website = &info.websites[0]
	}
	if handle := info.twitterHandle; handle != nil && *handle != "" {
		link := "https://x.com/" + *handle
		detail.Links.Twitter = &link
	}
	if handle := info.telegramHandle; handle != nil && *handle != "" {
		link := "https://t.me/" + *handle
		detail.Links.Telegram = &link
	}
	return detail
}

func priceChange24h(chart []MobileTokenChartPoint) *float64 {
	if len(chart) < 2 {
		return nil
	}
	first, last := chart[0].PriceUSD, chart[len(chart)-1].PriceUSD
	if math.IsNaN(first) || math.IsInf(first, 0) || math.IsNaN(last) || math.IsInf(last, 0) || first == 0 {
		return nil
	}
	change := (last - first) / first * 100
	return &change
}
