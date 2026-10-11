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

// kaminoReserveHistoryEndpoint is Kamino's hourly reserve metrics history
// (market, then reserve). Our own projection, kamino.reserve_updates, holds
// the same market price but covers AUTO only since 2026-09-20 (22 days on
// 2026-10-11), short of the window; and the selector reads current reserve
// state from Backyard's account view, not Timescale.
const kaminoReserveHistoryEndpoint = "https://api.kamino.finance/kamino-market/%s/reserves/%s/metrics/history"

// realizedYieldWindow is the trailing window a collateral's native yield is
// realized over: 30 days, the selector's horizon. The selector forecasts a
// lane held for 30 days, and the trailing 30 days are the realized record of
// that length.
//
// A 7-day figure does not also gate. On Kamino's history of 2026-10-11,
// moving the end point across the last 48 hourly points moved the 7-day
// figure by 0.5-0.7 points for syrupUSDC, AUTO and PRIME against 0.2 for the
// 30-day one; USDe read -4.1% over 7 days from peg noise alone (-0.7% over
// 30); and ONyc's price moves in NAV steps, so a 7-day window depends on how
// many steps it catches. A gate on that noise would flip lanes between samples
// while what they earn stays the same. A collateral that stops earning or
// loses value still lowers the 30-day figure in proportion.
const realizedYieldWindow = 30 * 24 * time.Hour

// realizedYieldMaxSpan bounds how far before the window start the oldest
// price may sit (a hole in the history): the span annualizes exactly, but a
// longer one is no longer the 30-day record.
const realizedYieldMaxSpan = realizedYieldWindow + 48*time.Hour

// realizedYieldRetry spaces history reads: the history advances hourly, so a
// reserve is read again once its newest point is an hour old, at most this
// often.
const realizedYieldRetry = 5 * time.Minute

// realizedYieldHistoryMaxBody bounds one reserve's history response (~1 MB
// for 32 hourly days).
const realizedYieldHistoryMaxBody = 8 << 20

// realizedYieldUnavailable labels a lane whose realized native yield cannot
// be measured; the lane has no economics. The feed's own yield is never
// substituted.
func realizedYieldUnavailable(detail string) error {
	return fmt.Errorf("realized_yield_unavailable(%s)", detail)
}

type reserveHistoryPoint struct {
	At    time.Time
	Price float64
}

// parseRealizedYield is the collateral's realized native yield: its reserve
// market price growth over the trailing window, annualized,
// (P_now/P_then)^(365 days/span) - 1.
//
// The price is metrics.assetPriceUSD, the reserve's stored market price: the
// price KLend values the collateral at for LTV (equal to the on-chain
// market_price_sf observations, e.g. AUTO 1.0237302007 at 2026-10-10 12:00).
// For syrupUSDC, ONyc, PRIME and AUTO it is the token's exchange rate in USD;
// for USDe its USD oracle. Nothing divides a peg out.
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

// fetchRealizedYield reads one collateral reserve's hourly history covering
// the window and its slack.
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

// realizedReserve is the feed's last read of one collateral reserve's
// history: the yield it measured (zero ObservedAt when none), the reason the
// last read failed, and when it was attempted.
type realizedReserve struct {
	yield     nativeYield
	reason    string
	attempted time.Time
}

// due reports a reserve whose history should be read again: never read, or
// its newest point an hour old (or none), at most every realizedYieldRetry.
func (r realizedReserve) due(now time.Time) bool {
	if r.attempted.IsZero() {
		return true
	}
	return now.Sub(r.attempted) >= realizedYieldRetry && (r.yield.ObservedAt.IsZero() || now.Sub(r.yield.ObservedAt) >= time.Hour)
}
