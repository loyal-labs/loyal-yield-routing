package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mr-tron/base58"
)

const maxRelayBody = 1 << 20

var EVMTxHash = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// A Solana signature is case-sensitive, so only an inbound hash is lowercased.
func OriginTx(d Direction, s string) (string, bool) {
	if d == DirectionOut {
		raw, err := base58.Decode(s)
		return s, err == nil && len(raw) == 64
	}
	s = strings.ToLower(s)
	return s, EVMTxHash.MatchString(s)
}

// EVMRPC holds one RPC URL per origin chain, used to simulate swap deposits;
// a chain without one gets no swap quotes.
type Relay struct {
	BaseURL string
	HTTP    *http.Client
	APIKey  string
	EVMRPC  map[Chain]string
}

func NewRelay() *Relay {
	return &Relay{BaseURL: "https://api.relay.link", HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (r *Relay) Quote(ctx context.Context, req QuoteRequest) (Quote, error) {
	net, tok, err := resolve(req)
	if err != nil {
		return Quote{}, err
	}
	body, err := json.Marshal(map[string]any{
		"user":                req.From,
		"recipient":           req.Recipient,
		"refundTo":            req.From,
		"originChainId":       net.ChainID,
		"destinationChainId":  SolanaRelayChainID,
		"originCurrency":      tok.Address,
		"destinationCurrency": SolanaUSDCMint,
		"amount":              req.AmountRaw.String(),
		"tradeType":           "EXACT_INPUT",
	})
	if err != nil {
		return Quote{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/quote/v2", bytes.NewReader(body))
	if err != nil {
		return Quote{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	raw, err := r.do(hreq)
	if err != nil {
		return Quote{}, fmt.Errorf("relay quote: %w", err)
	}
	q, err := CheckQuote(req, raw)
	if err != nil {
		return Quote{}, err
	}
	if err := net.checkMinimum(q.AmountInUSD); err != nil {
		return Quote{}, err
	}
	if q.minCredit != nil {
		if err := r.simulateCredit(ctx, net, req, tok, q); err != nil {
			return Quote{}, err
		}
	}
	return q, nil
}

type Status struct {
	State      string
	InTxHashes []string
	TxHashes   []string
	FailReason string
}

func (s Status) originTx(d Direction) string {
	for _, hash := range s.InTxHashes {
		if hash, ok := OriginTx(d, hash); ok {
			return hash
		}
	}
	return ""
}

func (r *Relay) Status(ctx context.Context, requestID string) (Status, error) {
	if requestID == "" {
		return Status{}, fmt.Errorf("relay status: empty request id")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.BaseURL+"/intents/status/v3?requestId="+url.QueryEscape(requestID), nil)
	if err != nil {
		return Status{}, err
	}
	raw, err := r.do(hreq)
	if err != nil {
		return Status{}, fmt.Errorf("relay status: %w", err)
	}
	var s struct {
		Status     string   `json:"status"`
		InTxHashes []string `json:"inTxHashes"`
		TxHashes   []string `json:"txHashes"`
		FailReason string   `json:"failReason"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("relay status: %w", err)
	}
	if s.Status == "" {
		return Status{}, fmt.Errorf("relay status: missing status")
	}
	if s.FailReason == "N/A" {
		s.FailReason = ""
	}
	return Status{State: s.Status, InTxHashes: s.InTxHashes, TxHashes: s.TxHashes, FailReason: s.FailReason}, nil
}

func (r *Relay) do(req *http.Request) ([]byte, error) {
	if r.APIKey != "" {
		req.Header.Set("x-api-key", r.APIKey)
	}
	res, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxRelayBody))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &HTTPError{Status: res.StatusCode, Body: raw, text: msg}
	}
	return raw, nil
}

type HTTPError struct {
	Status int
	Body   []byte
	text   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.text) }
