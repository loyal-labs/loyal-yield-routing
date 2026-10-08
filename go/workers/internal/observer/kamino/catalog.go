package kamino

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mr-tron/base58"
)

type CatalogClient struct {
	baseURL string
	http    *http.Client
}

func NewCatalogClient(baseURL string, timeout time.Duration) *CatalogClient {
	return &CatalogClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}}
}

type optionalFloat struct{ Value *float64 }

func (f *optionalFloat) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == `""` {
		return nil
	}
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		f.Value = &number
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	f.Value = &parsed
	return nil
}

type metricDTO struct {
	Reserve     *string       `json:"reserve"`
	Symbol      *string       `json:"liquidityToken"`
	Mint        *string       `json:"liquidityTokenMint"`
	SupplyAPY   optionalFloat `json:"supplyApy"`
	BorrowAPY   optionalFloat `json:"borrowApy"`
	TotalSupply optionalFloat `json:"totalSupplyUsd"`
	TotalBorrow optionalFloat `json:"totalBorrowUsd"`
}
type slotDurationDTO struct {
	Recent   optionalFloat `json:"recentSlotDurationInMs"`
	Median   optionalFloat `json:"medianSlotDurationMs"`
	Slot     optionalFloat `json:"slotDurationMs"`
	Duration optionalFloat `json:"duration"`
}

func (c *CatalogClient) Enrich(ctx context.Context, targets []Target) ([]Target, error) {
	markets := make(map[string][]metricDTO)
	for _, target := range targets {
		if target.Market == nil {
			return nil, fmt.Errorf("kamino target %s has no market identity", target.Reserve)
		}
		if _, loaded := markets[*target.Market]; loaded {
			continue
		}
		endpoint := fmt.Sprintf("%s/kamino-market/%s/reserves/metrics?env=mainnet-beta", c.baseURL, url.PathEscape(*target.Market))
		var metrics []metricDTO
		if err := c.get(ctx, endpoint, &metrics); err != nil {
			return nil, fmt.Errorf("fetch Kamino market %s: %w", *target.Market, err)
		}
		markets[*target.Market] = metrics
	}
	enriched := make([]Target, len(targets))
	for index, target := range targets {
		matches := 0
		for _, metric := range markets[*target.Market] {
			if metric.Reserve == nil || *metric.Reserve != target.Reserve {
				continue
			}
			matches++
			if metric.Mint != nil && target.LiquidityMint != nil && *metric.Mint != *target.LiquidityMint {
				return nil, fmt.Errorf("kamino API reserve %s mint %s does not match %s", target.Reserve, *metric.Mint, *target.LiquidityMint)
			}
			if metric.Symbol != nil && strings.TrimSpace(*metric.Symbol) != "" {
				target.Symbol = metric.Symbol
			}
			target.APISupplyAPY = metric.SupplyAPY.Value
			target.APIBorrowAPY = metric.BorrowAPY.Value
			target.APITotalSupplyUSD = metric.TotalSupply.Value
			target.APITotalBorrowUSD = metric.TotalBorrow.Value
		}
		if matches != 1 {
			return nil, fmt.Errorf("kamino API reserve %s resolved %d rows, expected one", target.Reserve, matches)
		}
		enriched[index] = target
	}
	return enriched, nil
}

// ObservationTargets is the watched reserve set: the stored catalog and Earn
// MAX targets enriched from the Kamino API, then the pinned supplemental
// reserves, which Rust appends without any API lookup (merge_observation_targets
// in kamino-reserve-monitor main.rs; a reserve resolved twice must keep one
// market/mint identity).
func (c *CatalogClient) ObservationTargets(ctx context.Context, stored []Target) ([]Target, error) {
	targets, err := c.Enrich(ctx, stored)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(targets))
	for position, target := range targets {
		index[target.Reserve] = position
	}
	for _, supplemental := range supplementalObservationTargets() {
		position, exists := index[supplemental.Reserve]
		if !exists {
			targets = append(targets, supplemental)
			continue
		}
		if !sameIdentity(targets[position].Market, supplemental.Market) || !sameIdentity(targets[position].LiquidityMint, supplemental.LiquidityMint) {
			return nil, fmt.Errorf("kamino reserve %s resolved conflicting observation identities", supplemental.Reserve)
		}
		targets[position] = supplemental
	}
	return targets, nil
}

func sameIdentity(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func (c *CatalogClient) SlotDuration(ctx context.Context) (float64, error) {
	var response slotDurationDTO
	if err := c.get(ctx, c.baseURL+"/slots/duration", &response); err != nil {
		return 0, err
	}
	for _, value := range []*float64{response.Recent.Value, response.Median.Value, response.Slot.Value, response.Duration.Value} {
		if value != nil && *value > 0 {
			return *value, nil
		}
	}
	return 0, fmt.Errorf("kamino slot duration response had no positive duration")
}
func (c *CatalogClient) get(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP request returned %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(target)
}

// SupportedReserveRecord is one row of the supported reserve catalog the
// planners read (loyal-kamino-codec SupportedReserveRecord).
type SupportedReserveRecord struct {
	Market, LiquidityMint, Reserve string
	MarketName, Symbol             string
	RiskBaskets                    []string
}

type supportedMarket struct {
	market, name string
	riskBaskets  []string
}

// policySupportedMarkets and policySupportedMints are loyal-kamino-data
// targets.rs policy_supported_markets and policy_supported_mints, in order.
var policySupportedMarkets = []supportedMarket{
	{"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF", "Main Market", []string{"safe", "medium", "aggressive"}},
	{"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "Figure Market", []string{"safe", "medium", "aggressive"}},
	{"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "Maple Market", []string{"safe", "medium", "aggressive"}},
	{"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "OnRe Market", []string{"safe", "medium", "aggressive"}},
	{"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF", "Ethena Market", []string{"safe", "medium", "aggressive"}},
	{"DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek", "JLP Market", []string{"medium", "aggressive"}},
	{"GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs", "Bitcoin Market", []string{"medium", "aggressive"}},
	{"CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw", "Superstate Opening Bell Market", []string{"medium", "aggressive"}},
	{"52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf", "Huma Market", []string{"aggressive"}},
	{"9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU", "Solstice Market", []string{"aggressive"}},
	{"5wJeMrUYECGq41fxRESKALVcHnNX26TAWy4W98yULsua", "xStocks Market", []string{"aggressive"}},
	{"ByYiZxp8QrdN9qbdtaAiePN8AAr3qvTPppNJDpf5DVJ5", "Altcoins Market", []string{"aggressive"}},
}

var policySupportedMints = map[string]string{
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD",
	"USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA":  "USDS",
	"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH": "USDG",
	"DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT": "USDE",
	"Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN": "SUSDE",
	"CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH": "CASH",
	"AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj": "SYRUPUSDC",
	"USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB":  "USD1",
	"9zNQRsGLjNKwCUU5Gq5LR8beUCPzQMVMqKAi3SSZh54u": "FDUSD",
	"AUSD1jCcCyPLybk1YnvPWsHQSrZ46dxwoMniN4N2UEB9": "AUSD",
	"3ThdFZQKM6kRyVGLG48kaPg5TRMhYMKY1iCRa9xop1WC": "EUSX",
	"BTRR3sj1Bn2ZjuemgbeQ6SCtf84iXS81CS7UDTSxUCaK": "USCC",
}

// SupportedReserves is KaminoApi::fetch_supported_reserves: for every policy
// market and policy mint, the API reserve with the largest totalSupplyUsd
// (first listed wins a tie), sorted by market, mint and reserve. Any market
// failure fails the whole catalog, so a partial response is never published.
func (c *CatalogClient) SupportedReserves(ctx context.Context) ([]SupportedReserveRecord, error) {
	type scored struct {
		record SupportedReserveRecord
		score  float64
	}
	byPair := make(map[[2]string]scored)
	for _, market := range policySupportedMarkets {
		endpoint := fmt.Sprintf("%s/kamino-market/%s/reserves/metrics?env=mainnet-beta", c.baseURL, url.PathEscape(market.market))
		var metrics []metricDTO
		if err := c.get(ctx, endpoint, &metrics); err != nil {
			return nil, fmt.Errorf("fetch supported market %s: %w", market.market, err)
		}
		for _, metric := range metrics {
			if metric.Reserve == nil || !validPublicKey(*metric.Reserve) || metric.Mint == nil || !validPublicKey(*metric.Mint) {
				continue
			}
			mintSymbol, supported := policySupportedMints[*metric.Mint]
			if !supported {
				continue
			}
			symbol := mintSymbol
			if metric.Symbol != nil {
				if normalized := strings.ToUpper(strings.TrimSpace(*metric.Symbol)); normalized != "" {
					symbol = normalized
				}
			}
			score := 0.0
			if metric.TotalSupply.Value != nil {
				score = *metric.TotalSupply.Value
			}
			pair := [2]string{market.market, *metric.Mint}
			if existing, exists := byPair[pair]; exists && score <= existing.score {
				continue
			}
			byPair[pair] = scored{SupportedReserveRecord{Market: market.market, LiquidityMint: *metric.Mint, Reserve: *metric.Reserve, MarketName: market.name, Symbol: symbol, RiskBaskets: append([]string(nil), market.riskBaskets...)}, score}
		}
	}
	records := make([]SupportedReserveRecord, 0, len(byPair))
	for _, value := range byPair {
		records = append(records, value.record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Market != records[j].Market {
			return records[i].Market < records[j].Market
		}
		if records[i].LiquidityMint != records[j].LiquidityMint {
			return records[i].LiquidityMint < records[j].LiquidityMint
		}
		return records[i].Reserve < records[j].Reserve
	})
	return records, nil
}

func validPublicKey(value string) bool {
	decoded, err := base58.Decode(value)
	return err == nil && len(decoded) == 32
}
