package earnmax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	voltrAPI      = "https://api.voltr.xyz/vault/"
	voltrHost     = "api.voltr.xyz"
	voltrTimeout  = 10 * time.Second
	voltrMaxBytes = 16 << 20
)

var voltrClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type DailyStat struct {
	Day    string
	TVLRaw float64
	LPRaw  float64
}

type SharePrice struct {
	PerLP      float64
	TotalValue *float64
}

func voltrGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, voltrTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, voltrAPI+Vault.String()+path, nil)
	if err != nil {
		return fmt.Errorf("%s request is invalid", voltrHost)
	}
	resp, err := voltrClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed", voltrHost)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %d", voltrHost, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, voltrMaxBytes+1))
	if err != nil {
		return fmt.Errorf("%s response unreadable", voltrHost)
	}
	if len(raw) > voltrMaxBytes {
		return fmt.Errorf("%s response too large", voltrHost)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s returned invalid JSON", voltrHost)
	}
	return nil
}

func FetchSharePrice(ctx context.Context) (SharePrice, error) {
	var body struct {
		Data struct {
			SharePrice *float64 `json:"sharePrice"`
			TotalValue *float64 `json:"totalValue"`
		} `json:"data"`
	}
	if err := voltrGet(ctx, "/share-price", &body); err != nil {
		return SharePrice{}, err
	}
	if body.Data.SharePrice == nil {
		return SharePrice{}, errors.New("Voltr returned no share price.")
	}
	return SharePrice{PerLP: *body.Data.SharePrice, TotalValue: body.Data.TotalValue}, nil
}

func FetchDailyStats(ctx context.Context) ([]DailyStat, error) {
	var body struct {
		Vault struct {
			DailyStats struct {
				DateLabels []*string  `json:"dateLabels"`
				TVLData    []*float64 `json:"tvlData"`
				LPData     []*float64 `json:"lpData"`
			} `json:"dailyStats"`
		} `json:"vault"`
	}
	if err := voltrGet(ctx, "", &body); err != nil {
		return nil, err
	}
	stats := body.Vault.DailyStats
	days := []DailyStat{}
	for i, day := range stats.DateLabels {
		if day == nil || i >= len(stats.TVLData) || i >= len(stats.LPData) || stats.TVLData[i] == nil || stats.LPData[i] == nil {
			continue
		}
		days = append(days, DailyStat{Day: *day, TVLRaw: *stats.TVLData[i], LPRaw: *stats.LPData[i]})
	}
	return days, nil
}
