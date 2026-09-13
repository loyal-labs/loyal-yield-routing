package kamino

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
