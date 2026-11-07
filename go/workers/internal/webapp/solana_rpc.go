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

	"github.com/mr-tron/base58"
	"golang.org/x/time/rate"
)

const (
	solanaRPCPath    = "/api/solana/rpc"
	solanaRPCTimeout = 30 * time.Second
	// The biggest browser batch is 60 getTransaction entries, about 260 bytes
	// each: getActivity with limit 30 (use-smart-account-sidebar-data.ts)
	// overfetches by 2 (default-activity-provider.ts).
	solanaRPCBatchEntries = 128
	solanaRPCBodyBytes    = 64 << 10
	solanaRPCInFlight     = 128
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
	solanaScan
	solanaSend
	solanaSubscribe
	solanaUnsubscribe
	solanaKeepalive
)

type solanaMethod struct {
	class solanaMethodClass
	// credits is the Helius price of one call, zero meaning 1.
	credits int
	// scope limits what a call may read or stream: nil allows any params.
	scope func(params []any) bool
}

// solanaMethods is the allowlist: a method absent here is refused, and a
// method is served only on the transport its class belongs to.
//
// Prices are helius.dev/docs/billing/credits, read Nov 6 2026: 1 credit by
// default, getProgramAccounts 10, DAS 10, sockets 2 per 0.1 MB.
//
// One address, per second: reads 3 (1 credit each, 259K a day), scans 0.2 (at
// most 173K credits a day), burst 300 reads, because one activity page is 3
// getSignaturesForAddress and 3 batches of 60 getTransaction. The class is by
// cost to serve, so getProgramAccountsV2 and getSignaturesForAddress are scans
// at 1 credit.
//
// The whole route, per second: 20 credits for calls and 512 KiB for sockets
// (10.5 credits), 2.6M credits or about $13 a day at worst, so no set of
// addresses can spend the key that sign-in shares. The Secure URL, of which the
// browser is a part, averaged 2.6 calls a second at its Nov 6 reading (9.4K an
// hour, reports/helius-rpc-cuts-2026-11-06.md), so 20 is 7 times the whole of
// it. The credit burst of 1200 is 6 activity pages at once.
var solanaMethods = map[string]solanaMethod{
	"getAccountInfo":                    {class: solanaRead},
	"getBalance":                        {class: solanaRead},
	"getBlockHeight":                    {class: solanaRead},
	"getEpochInfo":                      {class: solanaRead},
	"getFeeForMessage":                  {class: solanaRead},
	"getGenesisHash":                    {class: solanaRead},
	"getLatestBlockhash":                {class: solanaRead},
	"getMinimumBalanceForRentExemption": {class: solanaRead},
	"getMultipleAccounts":               {class: solanaRead},
	"getSignatureStatuses":              {class: solanaRead},
	"getSlot":                           {class: solanaRead},
	"getTokenAccountBalance":            {class: solanaRead},
	"getTokenAccountsByOwner":           {class: solanaRead},
	"getTransaction":                    {class: solanaRead},
	"simulateTransaction":               {class: solanaRead},

	"getAsset":                {class: solanaScan, credits: 10},
	"getAssetsByOwner":        {class: solanaScan, credits: 10},
	"getProgramAccounts":      {class: solanaScan, credits: 10, scope: programAccountsOfOneAddress},
	"getProgramAccountsV2":    {class: solanaScan, scope: programAccountsOfOneAddress},
	"getSignaturesForAddress": {class: solanaScan},

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

const (
	solanaTokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	solanaToken2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	solanaSmartAccounts    = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"
	solanaPrivateTransfer  = "97FzQdWi26mFNR21AbQNg4KqofiCLqQydQfAvRQMcXhV"
	solanaDelegation       = "DELeGGvXpWV2fqJUhqcF5ZSYMS4JTLjteaAMARRSaeSh"
)

// solanaBusyPrograms stream far more logs than a wallet. The list cannot name
// every busy program, so the socket byte budgets are what bound the bill.
var solanaBusyPrograms = map[string]bool{
	solanaTokenProgram:                             true,
	solanaToken2022Program:                         true,
	"11111111111111111111111111111111":             true,
	"ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL": true,
	"ComputeBudget111111111111111111111111111111":  true,
	"MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr":  true,
	"Vote111111111111111111111111111111111111111":  true,
	"Stake11111111111111111111111111111111111111":  true,
	"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4":  true,
	"675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8": true,
	"CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK": true,
	"whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc":  true,
	"LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo":  true,
	"6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P":  true,
}

func oneSolanaAddress(value any) (string, bool) {
	text, _ := value.(string)
	if solanaAddressOrEmpty(text) == "" {
		return "", false
	}
	decoded, err := base58.Decode(text)
	return text, err == nil && len(decoded) == 32
}

// filtersOnOneAddress holds when a filter pins the given offset to one 32-byte
// address. A shorter value matches most accounts, and an empty one all of them.
// web3.js adds encoding base58 to every memcmp, and base64 would make bytes a
// different, shorter value.
func filtersOnOneAddress(config any, offset string) bool {
	fields, _ := config.(map[string]any)
	filters, _ := fields["filters"].([]any)
	for _, filter := range filters {
		entry, _ := filter.(map[string]any)
		memcmp, _ := entry["memcmp"].(map[string]any)
		at, _ := memcmp["offset"].(json.Number)
		encoding, encoded := memcmp["encoding"]
		size := 2
		if encoded {
			size = 3
		}
		if at.String() != offset || len(memcmp) != size || (encoded && encoding != "base58") {
			continue
		}
		if _, ok := oneSolanaAddress(memcmp["bytes"]); ok {
			return true
		}
	}
	return false
}

// tokenAccountsOfOneOwner is the wallet's balance watch (frontend-asset-provider.ts).
func tokenAccountsOfOneOwner(params []any) bool {
	if len(params) < 2 {
		return false
	}
	program, _ := params[0].(string)
	return (program == solanaTokenProgram || program == solanaToken2022Program) && filtersOnOneAddress(params[1], "32")
}

// programAccountsOfOneAddress is every browser scan: policies of one settings
// account (smart-account-vaults client.ts) and deposits of one user
// (private-transactions getAllDepositsByUser), the owner at offset 8 in all.
func programAccountsOfOneAddress(params []any) bool {
	if len(params) < 2 {
		return false
	}
	switch program, _ := params[0].(string); program {
	case solanaSmartAccounts, solanaPrivateTransfer, solanaDelegation:
		return filtersOnOneAddress(params[1], "8")
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
	if len(filter) != 1 || len(mentions) != 1 {
		return false
	}
	address, ok := oneSolanaAddress(mentions[0])
	return ok && !solanaBusyPrograms[address]
}

var (
	solanaReads        = routeClass{name: "solana-reads", client: budget{every: time.Second / 3, burst: 300}}
	solanaScans        = routeClass{name: "solana-scans", client: budget{every: 5 * time.Second, burst: 60}}
	solanaSends        = routeClass{name: "solana-sends", client: budget{every: 2 * time.Second, burst: 30}}
	solanaSocketFrames = routeClass{name: "solana-socket-frames", client: budget{every: time.Second / 10, burst: 300}}
	solanaSocketOpens  = routeClass{name: "solana-socket-opens", client: budget{every: time.Second, burst: 60}}
	solanaRouteCredits = budget{every: time.Second / 20, burst: 1200}
)

// SolanaRPC serves the browser's mainnet RPC, so the upstream URL stays on the server.
type SolanaRPC struct {
	endpoint    string
	socketURL   string
	environment string
	client      *http.Client
	buckets     *limiters
	credits     *rate.Limiter
	creditsDry  rate.Sometimes
	inFlight    chan struct{}
	socketBytes *rate.Limiter

	socketsMu    sync.Mutex
	sockets      map[string]int
	socketsTotal int
	socketsOpen  sync.WaitGroup
	closing      chan struct{}
	closingNow   bool
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
		buckets:     newLimiters(),
		credits:     rate.NewLimiter(rate.Every(solanaRouteCredits.every), solanaRouteCredits.burst),
		creditsDry:  rate.Sometimes{Interval: time.Minute},
		inFlight:    make(chan struct{}, solanaRPCInFlight),
		socketBytes: rate.NewLimiter(solanaRouteSocketBytes, solanaRouteSocketBurst),
		sockets:     map[string]int{},
		closing:     make(chan struct{}),
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
	if name, code, message, refused := refusedCall(calls); refused {
		logSolanaRefusal("http", "method", name)
		if !batch {
			writeSolanaRefusal(w, refuseSolanaCall(calls[0].ID, code, message))
			return
		}
		refusals := make([]solanaRPCRefusal, len(calls))
		for i, call := range calls {
			refusals[i] = refuseSolanaCall(call.ID, code, message)
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
	select {
	case s.inFlight <- struct{}{}:
		defer func() { <-s.inFlight }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	if !s.spendRouteCredits(w, calls) {
		return
	}
	s.forwardCalls(w, r, payload)
}

// refusedCall checks every call and narrows scoped params to what scope read,
// so upstream parses the same thing whatever duplicate keys the body held.
func refusedCall(calls []solanaCall) (string, int, string, bool) {
	for i := range calls {
		name := calls[i].Method
		method := solanaMethods[name]
		switch method.class {
		case solanaRead, solanaScan, solanaSend:
		default:
			return name, jsonRPCMethodNotFound, "method not allowed: " + clipSolanaLog(name), true
		}
		if method.scope == nil {
			continue
		}
		params, ok := parseSolanaParams(calls[i].Params)
		if !ok || !method.scope(params) {
			return name, jsonRPCInvalidParams, "params not allowed: " + name, true
		}
		encoded, err := json.Marshal(params)
		if err != nil {
			return name, jsonRPCInvalidParams, "params not allowed: " + name, true
		}
		calls[i].Params = encoded
	}
	return "", 0, "", false
}

func (s *SolanaRPC) spendCalls(w http.ResponseWriter, r *http.Request, calls []solanaCall) bool {
	scans, sends := 0, 0
	for _, call := range calls {
		switch solanaMethods[call.Method].class {
		case solanaScan:
			scans++
		case solanaSend:
			sends++
		}
	}
	ip := clientKey(r)
	return s.spend(w, solanaReads, ip, len(calls)-scans-sends) && s.spend(w, solanaScans, ip, scans) && s.spend(w, solanaSends, ip, sends)
}

func (s *SolanaRPC) spendRouteCredits(w http.ResponseWriter, calls []solanaCall) bool {
	credits := 0
	for _, call := range calls {
		credits += max(solanaMethods[call.Method].credits, 1)
	}
	now := time.Now()
	if !s.credits.AllowN(now, credits) {
		s.creditsDry.Do(func() { slog.Warn("solana rpc route budget is empty") })
		tooMany(w, s.credits, solanaRouteCredits, now)
		return false
	}
	return true
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
