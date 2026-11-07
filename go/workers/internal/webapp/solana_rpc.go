package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	solanaRPCPath    = "/api/solana/rpc"
	solanaRPCTimeout = 30 * time.Second
	// The biggest browser batch is 60 getTransaction entries, about 260 bytes
	// each: getActivity with limit 30 (use-smart-account-sidebar-data.ts)
	// overfetches by 2 (default-activity-provider.ts).
	solanaRPCBatchEntries = 128
	solanaRPCBodyBytes    = 64 << 10
)

const (
	jsonRPCParseError     = -32700
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
	jsonRPCInternalError  = -32603
	jsonRPCServerError    = -32000
)

type solanaMethodClass int

const (
	solanaRead solanaMethodClass = iota + 1
	solanaSend
	solanaSubscribe
	solanaUnsubscribe
	solanaKeepalive
)

type solanaMethod struct {
	class solanaMethodClass
	// scope limits what a subscription may stream, because Helius bills
	// sockets by data: nil allows any params.
	scope func(params []any) bool
}

// solanaMethods is the allowlist: a method absent here is refused, and a
// method is served only on the transport its class belongs to.
var solanaMethods = map[string]solanaMethod{
	"getAccountInfo":                    {class: solanaRead},
	"getAsset":                          {class: solanaRead},
	"getAssetsByOwner":                  {class: solanaRead},
	"getBalance":                        {class: solanaRead},
	"getBlockHeight":                    {class: solanaRead},
	"getEpochInfo":                      {class: solanaRead},
	"getFeeForMessage":                  {class: solanaRead},
	"getGenesisHash":                    {class: solanaRead},
	"getLatestBlockhash":                {class: solanaRead},
	"getMinimumBalanceForRentExemption": {class: solanaRead},
	"getMultipleAccounts":               {class: solanaRead},
	"getProgramAccounts":                {class: solanaRead},
	"getProgramAccountsV2":              {class: solanaRead},
	"getSignatureStatuses":              {class: solanaRead},
	"getSignaturesForAddress":           {class: solanaRead},
	"getSlot":                           {class: solanaRead},
	"getTokenAccountBalance":            {class: solanaRead},
	"getTokenAccountsByOwner":           {class: solanaRead},
	"getTransaction":                    {class: solanaRead},
	"simulateTransaction":               {class: solanaRead},

	"sendTransaction": {class: solanaSend},

	"accountSubscribe":     {class: solanaSubscribe},
	"accountUnsubscribe":   {class: solanaUnsubscribe},
	"logsSubscribe":        {class: solanaSubscribe, scope: logsMentioningOneAddress},
	"logsUnsubscribe":      {class: solanaUnsubscribe},
	"programSubscribe":     {class: solanaSubscribe, scope: tokenAccountsOfOneOwner},
	"programUnsubscribe":   {class: solanaUnsubscribe},
	"signatureSubscribe":   {class: solanaSubscribe},
	"signatureUnsubscribe": {class: solanaUnsubscribe},
	"slotSubscribe":        {class: solanaSubscribe},
	"slotUnsubscribe":      {class: solanaUnsubscribe},

	// web3.js and @solana/kit send this every 5 seconds to keep a socket from
	// idling out. The relay pings both sides itself, so it never goes upstream.
	"ping": {class: solanaKeepalive},
}

// tokenAccountsOfOneOwner is the wallet's balance watch (frontend-asset-provider.ts).
func tokenAccountsOfOneOwner(params []any) bool {
	if len(params) < 2 {
		return false
	}
	if program, _ := params[0].(string); program != "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" && program != "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb" {
		return false
	}
	config, _ := params[1].(map[string]any)
	filters, _ := config["filters"].([]any)
	for _, filter := range filters {
		entry, _ := filter.(map[string]any)
		memcmp, _ := entry["memcmp"].(map[string]any)
		if offset, _ := memcmp["offset"].(json.Number); offset == "32" {
			return true
		}
	}
	return false
}

// logsMentioningOneAddress is the wallet's activity watch (default-activity-provider.ts).
func logsMentioningOneAddress(params []any) bool {
	if len(params) == 0 {
		return false
	}
	filter, _ := params[0].(map[string]any)
	mentions, _ := filter["mentions"].([]any)
	return len(filter) == 1 && len(mentions) == 1
}

var (
	solanaReads = routeClass{name: "solana-reads", client: budget{every: time.Second / 10, burst: 300}}
	solanaSends = routeClass{name: "solana-sends", client: budget{every: 2 * time.Second, burst: 30}}
)

// SolanaRPC serves the browser's mainnet RPC, so the upstream URL stays on the server.
type SolanaRPC struct {
	endpoint    string
	socketURL   string
	environment string
	client      *http.Client
	buckets     *limiters

	socketsMu sync.Mutex
	sockets   map[string]int
}

func NewSolanaRPC(endpoint, environment string) (*SolanaRPC, error) {
	upstream, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") {
		return nil, errors.New("WEB_SOLANA_RPC_URL is not a usable RPC URL")
	}
	httpURL := upstream.String()
	if upstream.Scheme == "https" {
		upstream.Scheme = "wss"
	} else {
		upstream.Scheme = "ws"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	return &SolanaRPC{
		endpoint:    httpURL,
		socketURL:   upstream.String(),
		environment: environment,
		client: &http.Client{
			Timeout:   solanaRPCTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		buckets: newLimiters(),
		sockets: map[string]int{},
	}, nil
}

func (s *SolanaRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.serveCalls(w, r)
	case http.MethodGet:
		s.serveSocket(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

// solanaCall holds the only fields that reach upstream.
type solanaCall struct {
	JSONRPC json.RawMessage `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type solanaRPCFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type solanaRPCRefusal struct {
	JSONRPC string           `json:"jsonrpc"`
	Error   solanaRPCFailure `json:"error"`
	ID      json.RawMessage  `json:"id"`
}

func refuseSolanaCall(id json.RawMessage, code int, message string) solanaRPCRefusal {
	return solanaRPCRefusal{JSONRPC: "2.0", Error: solanaRPCFailure{Code: code, Message: message}, ID: id}
}

// parseSolanaCall looks keys up exactly. A struct decode matches keys
// case-insensitively with the last duplicate winning, so
// {"method":"getHealth","Method":"getAccountInfo"} would pass the table as one
// method and reach upstream as another.
func parseSolanaCall(raw []byte) (solanaCall, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return solanaCall{}, false
	}
	call := solanaCall{JSONRPC: fields["jsonrpc"], ID: fields["id"], Params: fields["params"]}
	if json.Unmarshal(fields["method"], &call.Method) != nil || call.Method == "" {
		return solanaCall{ID: fields["id"]}, false
	}
	return call, true
}

// parseSolanaBody answers the calls, whether the body was a batch, and the
// refusal when the body is malformed.
func parseSolanaBody(body []byte) ([]solanaCall, bool, *solanaRPCRefusal) {
	malformed := func(code int, message string) ([]solanaCall, bool, *solanaRPCRefusal) {
		refusal := refuseSolanaCall(nil, code, message)
		return nil, false, &refusal
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		call, ok := parseSolanaCall(body)
		if !ok {
			if !json.Valid(body) {
				return malformed(jsonRPCParseError, "parse error")
			}
			refusal := refuseSolanaCall(call.ID, jsonRPCInvalidRequest, "invalid request")
			return nil, false, &refusal
		}
		return []solanaCall{call}, false, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(body, &entries) != nil {
		return malformed(jsonRPCParseError, "parse error")
	}
	if len(entries) == 0 || len(entries) > solanaRPCBatchEntries {
		return malformed(jsonRPCInvalidRequest, "batch length is out of range")
	}
	calls := make([]solanaCall, len(entries))
	for i, entry := range entries {
		var ok bool
		if calls[i], ok = parseSolanaCall(entry); !ok {
			return malformed(jsonRPCInvalidRequest, "invalid request in batch")
		}
	}
	return calls, true, nil
}

func (s *SolanaRPC) serveCalls(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, solanaRPCBodyBytes))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	calls, batch, refusal := parseSolanaBody(body)
	if refusal != nil {
		writeSolanaRefusal(w, *refusal)
		return
	}
	// Refused calls spend tokens too, which bounds the refusal log per client.
	if !s.spendCalls(w, r, calls) {
		return
	}
	if refused, ok := firstRefusedCall(calls); ok {
		logSolanaRefusal("http", "method", refused)
		message := "method not allowed: " + clipSolanaLog(refused)
		if !batch {
			writeSolanaRefusal(w, refuseSolanaCall(calls[0].ID, jsonRPCMethodNotFound, message))
			return
		}
		refusals := make([]solanaRPCRefusal, len(calls))
		for i, call := range calls {
			refusals[i] = refuseSolanaCall(call.ID, jsonRPCMethodNotFound, message)
		}
		writeSolanaRefusal(w, refusals)
		return
	}
	var payload []byte
	if batch {
		payload, err = json.Marshal(calls)
	} else {
		payload, err = json.Marshal(calls[0])
	}
	if err != nil {
		writeSolanaRefusal(w, refuseSolanaCall(nil, jsonRPCParseError, "parse error"))
		return
	}
	s.forwardCalls(w, r, payload)
}

func firstRefusedCall(calls []solanaCall) (string, bool) {
	for _, call := range calls {
		if class := solanaMethods[call.Method].class; class != solanaRead && class != solanaSend {
			return call.Method, true
		}
	}
	return "", false
}

func (s *SolanaRPC) spendCalls(w http.ResponseWriter, r *http.Request, calls []solanaCall) bool {
	sends := 0
	for _, call := range calls {
		if solanaMethods[call.Method].class == solanaSend {
			sends++
		}
	}
	ip := clientIP(r)
	return s.spend(w, solanaReads, ip, len(calls)-sends) && s.spend(w, solanaSends, ip, sends)
}

func (s *SolanaRPC) spend(w http.ResponseWriter, class routeClass, ip string, tokens int) bool {
	if tokens == 0 {
		return true
	}
	now := time.Now()
	limiter := s.buckets.get(limiterKey{class.name, "client", ip}, class.client, now)
	if !limiter.AllowN(now, tokens) {
		tooMany(w, limiter, class.client, now)
		return false
	}
	return true
}

func (s *SolanaRPC) forwardCalls(w http.ResponseWriter, r *http.Request, payload []byte) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		slog.Error("solana rpc upstream request", "error", "cannot build the request")
		writeSolanaUpstreamFailure(w)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		if r.Context().Err() == nil {
			slog.Error("solana rpc upstream", "error", solanaUpstreamFailure(err))
		}
		writeSolanaUpstreamFailure(w)
		return
	}
	defer func() { _ = response.Body.Close() }()
	for _, name := range []string{"Content-Type", "Retry-After"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

// solanaUpstreamFailure names the failure without its text: an http.Client
// error quotes the request URL, which carries the upstream key.
func solanaUpstreamFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unreachable"
}

func writeSolanaUpstreamFailure(w http.ResponseWriter) {
	body, _ := json.Marshal(refuseSolanaCall(nil, jsonRPCInternalError, "upstream unavailable"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
}

func writeSolanaRefusal(w http.ResponseWriter, refusal any) {
	body, err := json.Marshal(refusal)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func logSolanaRefusal(transport, kind, value string) {
	slog.Warn("solana rpc refused", "transport", transport, kind, clipSolanaLog(value))
}

func clipSolanaLog(value string) string {
	if len(value) > 64 {
		return value[:64]
	}
	return value
}
