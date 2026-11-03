package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	RequestsBatch    = 50
	requestsPage     = 20
	requestsMaxPages = 25
)

// DepositRequest is one Relay request on an open deposit address, as Relay
// reports it.
type DepositRequest struct {
	ID             string
	Status         string
	DepositAddress string
	Recipient      string
	// OriginTx is the request's own origin-chain transaction, not Relay's
	// depositAddress.depositTxHash, which names the address's first deposit on
	// every later request to the same address.
	OriginTx       string
	OriginCurrency string
	OutputChainID  int64
	OutputCurrency string
	AmountInRaw    *big.Int
	AmountOutRaw   *big.Int
	CreatedAt      time.Time
}

type relayRequestRoute struct {
	Origin struct {
		InputCurrency relayCurrencyAmount `json:"inputCurrency"`
	} `json:"origin"`
	Destination struct {
		OutputCurrency relayCurrencyAmount `json:"outputCurrency"`
	} `json:"destination"`
}

type relayRequest struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Recipient      string `json:"recipient"`
	DepositAddress *struct {
		Address string `json:"address"`
	} `json:"depositAddress"`
	Data struct {
		InTxs []struct {
			TxHash  string `json:"txHash"`
			ChainID int64  `json:"chainId"`
		} `json:"inTxs"`
		Route struct {
			Actual *relayRequestRoute `json:"actual"`
			Quoted *relayRequestRoute `json:"quoted"`
		} `json:"route"`
	} `json:"data"`
	CreatedAt time.Time `json:"createdAt"`
}

type relayRequests struct {
	Requests     []relayRequest `json:"requests"`
	Continuation string         `json:"continuation"`
}

func (q relayRequest) parse(originChainID int64) DepositRequest {
	d := DepositRequest{ID: q.ID, Status: q.Status, Recipient: q.Recipient, CreatedAt: q.CreatedAt}
	if q.DepositAddress != nil {
		d.DepositAddress = q.DepositAddress.Address
	}
	for _, tx := range q.Data.InTxs {
		if tx.ChainID == originChainID && tx.TxHash != "" {
			d.OriginTx = tx.TxHash
			break
		}
	}
	route := q.Data.Route.Actual
	if route == nil {
		route = q.Data.Route.Quoted
	}
	if route != nil {
		d.OriginCurrency = route.Origin.InputCurrency.Currency.Address
		d.OutputChainID = route.Destination.OutputCurrency.Currency.ChainID
		d.OutputCurrency = route.Destination.OutputCurrency.Currency.Address
		d.AmountInRaw, _ = parseAmount(route.Origin.InputCurrency.Amount)
		d.AmountOutRaw, _ = parseAmount(route.Destination.OutputCurrency.Amount)
	}
	return d
}

// Requests lists the Relay requests created since since on up to
// RequestsBatch deposit addresses of one origin chain, following Relay's
// continuation to the last page.
func (r *Relay) Requests(ctx context.Context, originChainID int64, depositAddresses []string, since time.Time) ([]DepositRequest, error) {
	if len(depositAddresses) == 0 || len(depositAddresses) > RequestsBatch {
		return nil, fmt.Errorf("relay requests: %d deposit addresses, want 1 to %d", len(depositAddresses), RequestsBatch)
	}
	filters, err := json.Marshal([]map[string][]string{{"depositAddress": depositAddresses}})
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"filters":                {string(filters)},
		"includePendingRequests": {"true"},
		"startTimestamp":         {strconv.FormatInt(since.Unix(), 10)},
		"limit":                  {strconv.Itoa(requestsPage)},
	}
	var out []DepositRequest
	for range requestsMaxPages {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+"/requests/v3?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		raw, err := r.do(hreq)
		if err != nil {
			return nil, fmt.Errorf("relay requests: %w", err)
		}
		var page relayRequests
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("relay requests: %w", err)
		}
		for _, q := range page.Requests {
			out = append(out, q.parse(originChainID))
		}
		if page.Continuation == "" || len(page.Requests) == 0 {
			return out, nil
		}
		query.Set("continuation", page.Continuation)
	}
	return nil, fmt.Errorf("relay requests: more than %d pages", requestsMaxPages)
}

func (r *Relay) Request(ctx context.Context, originChainID int64, requestID string) (DepositRequest, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+"/requests/v3?"+url.Values{"id": {requestID}}.Encode(), nil)
	if err != nil {
		return DepositRequest{}, err
	}
	raw, err := r.do(hreq)
	if err != nil {
		return DepositRequest{}, fmt.Errorf("relay request: %w", err)
	}
	var page relayRequests
	if err := json.Unmarshal(raw, &page); err != nil {
		return DepositRequest{}, fmt.Errorf("relay request: %w", err)
	}
	if len(page.Requests) != 1 || page.Requests[0].ID != requestID {
		return DepositRequest{}, fmt.Errorf("relay request %s: %d requests returned", requestID, len(page.Requests))
	}
	return page.Requests[0].parse(originChainID), nil
}
