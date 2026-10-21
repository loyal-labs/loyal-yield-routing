package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const kaminoReserveHistoryEndpoint = "https://api.kamino.finance/kamino-market/%s/reserves/%s/metrics/history"

const realizedYieldWindow = selectorHorizon

const realizedYieldMaxSpan = realizedYieldWindow + 48*time.Hour

const realizedYieldRetry = 5 * time.Minute

const realizedYieldHistoryMaxBody = 8 << 20

func realizedYieldUnavailable(detail string) error {
	return fmt.Errorf("realized_yield_unavailable(%s)", detail)
}

type reserveHistoryPoint struct {
	At    time.Time
	Price float64
}

func parseRealizedYield(body []byte, reserve, mint string, now time.Time) (nativeYield, error) {
	var history struct {
		Reserve string `json:"reserve"`
		History []struct {
			Timestamp time.Time `json:"timestamp"`
			Metrics   struct {
				MintAddress   string `json:"mintAddress"`
				AssetPriceUSD string `json:"assetPriceUSD"`
			} `json:"metrics"`
		} `json:"history"`
	}
	if err := json.Unmarshal(body, &history); err != nil {
		return nativeYield{}, realizedYieldUnavailable("history_invalid")
	}
	if history.Reserve != reserve {
		return nativeYield{}, realizedYieldUnavailable("history_identity_mismatch")
	}
	points := make([]reserveHistoryPoint, 0, len(history.History))
	for _, h := range history.History {
		if h.Metrics.MintAddress != mint {
			return nativeYield{}, realizedYieldUnavailable("history_identity_mismatch")
		}
		price, err := strconv.ParseFloat(h.Metrics.AssetPriceUSD, 64)
		if err != nil || !finite(price) || price <= 0 || h.Timestamp.IsZero() || h.Timestamp.After(now) {
			continue
		}
		points = append(points, reserveHistoryPoint{h.Timestamp, price})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	if len(points) < 2 {
		return nativeYield{}, realizedYieldUnavailable("history_short")
	}
	latest := points[len(points)-1]
	start := latest.At.Add(-realizedYieldWindow)
	i := sort.Search(len(points), func(i int) bool { return points[i].At.After(start) }) - 1
	if i < 0 {
		return nativeYield{}, realizedYieldUnavailable("history_short")
	}
	then := points[i]
	span := latest.At.Sub(then.At)
	if span > realizedYieldMaxSpan {
		return nativeYield{}, realizedYieldUnavailable("history_gap")
	}
	apy := math.Pow(latest.Price/then.Price, float64(365*24*time.Hour)/float64(span)) - 1
	if !finite(apy) || apy <= -1 || apy > 10 {
		return nativeYield{}, realizedYieldUnavailable("history_price_invalid")
	}
	return nativeYield{APY: apy, ObservedAt: latest.At, EvidenceID: sha256Bytes(body)}, nil
}

func fetchRealizedYield(ctx context.Context, client *http.Client, endpoint string, route RuntimeRoute, now time.Time) (nativeYield, error) {
	if client == nil {
		return nativeYield{}, realizedYieldUnavailable("history_client_unavailable")
	}
	u, err := url.Parse(fmt.Sprintf(endpoint, url.PathEscape(route.Kamino.Market), url.PathEscape(route.Kamino.CollateralReserve)))
	if err != nil {
		return nativeYield{}, realizedYieldUnavailable("history_endpoint_invalid")
	}
	q := u.Query()
	q.Set("env", "mainnet-beta")
	q.Set("frequency", "hour")
	q.Set("start", now.Add(-realizedYieldMaxSpan).UTC().Format(time.RFC3339))
	q.Set("end", now.UTC().Format(time.RFC3339))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nativeYield{}, realizedYieldUnavailable("history_request_invalid")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nativeYield{}, realizedYieldUnavailable("history_unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nativeYield{}, realizedYieldUnavailable(fmt.Sprintf("history_http_%d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, realizedYieldHistoryMaxBody+1))
	if err != nil || len(body) > realizedYieldHistoryMaxBody {
		return nativeYield{}, realizedYieldUnavailable("history_body_unavailable")
	}
	return parseRealizedYield(body, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, now)
}

type realizedReserve struct {
	yield     nativeYield
	reason    string
	attempted time.Time
}

func (r realizedReserve) due(now time.Time) bool {
	if r.attempted.IsZero() {
		return true
	}
	return now.Sub(r.attempted) >= realizedYieldRetry && (r.yield.ObservedAt.IsZero() || now.Sub(r.yield.ObservedAt) >= time.Hour)
}
