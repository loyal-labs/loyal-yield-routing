package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

const (
	// One wallet tab opens 3 sockets, and past the cap a tab cannot confirm
	// its sends, so this allows 8 tabs behind one address.
	solanaSocketsPerClient = 24
	// Worst case, per socket: 47 KiB idle (measured), a 64 KiB upstream frame and
	// a 16 KiB client frame in Go, 4 kernel buffers of 32 KiB (Linux doubles the
	// asked 16). At 512 sockets that is 64 MiB of Go and 64 MiB of kernel. 128 calls
	// in flight add 13 MiB. MemoryMax is 384 MiB: 288 go to Go (GOMEMLIMIT in
	// main.go), 64 to the kernel, 32 spare, which leaves the site 211 MiB of Go.
	solanaSocketsTotal           = 512
	solanaSubscriptionsPerSocket = 32
	solanaSocketFrameBytes       = 16 << 10
	// The largest browser subscription is logsNotification: the runtime caps a
	// transaction's logs at 10,000 bytes, and JSON escapes a byte to at most 6,
	// so 60,000 bytes and an envelope. Account watches (wallet, token accounts)
	// are far smaller. An attacker can own a 10 MiB account, so the limit is real.
	solanaUpstreamFrameBytes = 64 << 10
	// Helius bills a socket by data. A proof socket with slot, account, program
	// and busy logs watches together took 36 to 50 KB/s, so a wallet has room. The
	// route budget is what keeps 512 sockets from billing 64 KiB/s each.
	solanaSocketBytes       rate.Limit = 64 << 10
	solanaSocketBurst                  = 256 << 10
	solanaRouteSocketBytes  rate.Limit = 512 << 10
	solanaRouteSocketBurst             = 4 << 20
	solanaSocketBufferBytes            = 16 << 10
	// A subscribe reply is a short id and integer, so a longer upstream frame
	// is a notification with nothing the peek needs.
	solanaSubscribeReplyBytes = 1 << 10
	solanaSocketDialWait      = 10 * time.Second
	solanaSocketWriteWait     = 10 * time.Second
	solanaSocketPingEvery     = 30 * time.Second
	solanaSocketReadWait      = 75 * time.Second
)

var (
	solanaSocketUpgrader = websocket.Upgrader{
		CheckOrigin:     func(*http.Request) bool { return true },
		ReadBufferSize:  1 << 10,
		WriteBufferSize: 1 << 10,
	}
	solanaSocketDialer = websocket.Dialer{
		HandshakeTimeout: solanaSocketDialWait,
		ReadBufferSize:   1 << 10,
		WriteBufferSize:  1 << 10,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err == nil {
				boundSocketBuffers(conn)
			}
			return conn, err
		},
	}
)

// boundSocketBuffers keeps kernel memory in the arithmetic: the kernel charges
// socket buffers to the unit and autotunes them to megabytes.
func boundSocketBuffers(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(solanaSocketBufferBytes)
		_ = tcp.SetWriteBuffer(solanaSocketBufferBytes)
	}
}

func (s *SolanaRPC) serveSocket(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
		return
	}
	ip := clientKey(r)
	if !s.acquireSocket(ip) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	defer s.releaseSocket(ip)
	if !s.spend(w, solanaSocketOpens, ip, 1) {
		return
	}
	if origin := r.Header.Get("Origin"); !s.allowedSocketOrigin(origin) {
		logSolanaRefusal("socket", "origin", origin)
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	dial, cancel := context.WithTimeout(r.Context(), solanaSocketDialWait)
	defer cancel()
	upstream, _, err := solanaSocketDialer.DialContext(dial, s.socketURL, nil)
	if err != nil {
		slog.Error("solana rpc upstream socket", "error", solanaUpstreamFailure(err))
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	client, err := solanaSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	boundSocketBuffers(client.NetConn())
	client.SetReadLimit(solanaSocketFrameBytes)
	upstream.SetReadLimit(solanaUpstreamFrameBytes)
	socket := &solanaSocket{rpc: s, ip: ip, client: client, upstream: upstream, subscriptions: newSolanaSubscriptions()}
	socket.run()
}

// CloseSockets sends every browser socket a close frame, so clients reconnect
// at once instead of waiting out a cut connection. http.Server.Shutdown does
// not reach hijacked connections.
func (s *SolanaRPC) CloseSockets(ctx context.Context) {
	s.socketsMu.Lock()
	if !s.closingNow {
		s.closingNow = true
		close(s.closing)
	}
	s.socketsMu.Unlock()
	drained := make(chan struct{})
	go func() {
		s.socketsOpen.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
	}
}

// allowedSocketOrigin stops other sites' pages from using the route. A
// non-browser client can spoof this, so the method table and caps are the limit.
func (s *SolanaRPC) allowedSocketOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "https" && (host == "askloyal.com" || strings.HasSuffix(host, ".askloyal.com")) {
		return true
	}
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	return s.environment != "prod" && local && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func (s *SolanaRPC) acquireSocket(ip string) bool {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()
	if s.closingNow || s.socketsTotal >= solanaSocketsTotal || s.sockets[ip] >= solanaSocketsPerClient {
		return false
	}
	s.sockets[ip]++
	s.socketsTotal++
	s.socketsOpen.Add(1)
	return true
}

func (s *SolanaRPC) releaseSocket(ip string) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()
	if s.sockets[ip]--; s.sockets[ip] <= 0 {
		delete(s.sockets, ip)
	}
	s.socketsTotal--
	s.socketsOpen.Done()
}

type solanaSocket struct {
	rpc              *SolanaRPC
	ip               string
	client, upstream *websocket.Conn
	clientWrites     sync.Mutex
	subscriptions    *solanaSubscriptions
}

// run returns once both sides are closed and its goroutines have ended.
func (s *solanaSocket) run() {
	ctx, cancel := context.WithCancel(context.Background())
	end := sync.OnceFunc(func() {
		cancel()
		_ = s.client.Close()
		_ = s.upstream.Close()
	})
	var relays sync.WaitGroup
	relays.Go(func() {
		defer end()
		s.relayUpstream(ctx)
	})
	relays.Go(func() {
		defer end()
		s.ping(ctx)
	})
	s.relayClient(ctx)
	end()
	relays.Wait()
}

func awaitSocketPeer(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(solanaSocketReadWait))
}

func (s *solanaSocket) relayClient(ctx context.Context) {
	defer closeSolanaSocket(s.upstream, websocket.CloseNormalClosure, "")
	awaitSocketPeer(s.client)
	s.client.SetPongHandler(func(string) error {
		awaitSocketPeer(s.client)
		return nil
	})
	for {
		kind, frame, err := s.client.ReadMessage()
		if err != nil {
			return
		}
		// A frame waits rather than being refused: web3.js retries a refused
		// subscribe at once, so a refusal would only make the storm.
		frames := s.rpc.buckets.get(limiterKey{solanaSocketFrames.name, "client", s.ip}, solanaSocketFrames.client, time.Now())
		if frames.Wait(ctx) != nil {
			return
		}
		awaitSocketPeer(s.client)
		payload, refusal := s.admit(kind, frame)
		if refusal != nil {
			body, err := json.Marshal(refusal)
			if err != nil || s.writeClient(websocket.TextMessage, body) != nil {
				return
			}
			continue
		}
		if payload == nil {
			continue
		}
		_ = s.upstream.SetWriteDeadline(time.Now().Add(solanaSocketWriteWait))
		if s.upstream.WriteMessage(websocket.TextMessage, payload) != nil {
			return
		}
	}
}

// relayUpstream never forwards a normal close: rpc-websockets does not
// reconnect after code 1000, and a browser must reconnect when upstream ends.
func (s *solanaSocket) relayUpstream(ctx context.Context) {
	defer closeSolanaSocket(s.client, websocket.CloseGoingAway, "upstream closed")
	awaitSocketPeer(s.upstream)
	s.upstream.SetPongHandler(func(string) error {
		awaitSocketPeer(s.upstream)
		return nil
	})
	spent := rate.NewLimiter(solanaSocketBytes, solanaSocketBurst)
	for {
		kind, frame, err := s.upstream.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) {
				slog.Warn("solana rpc upstream frame over the limit")
			}
			return
		}
		s.subscriptions.observe(frame)
		if s.writeClient(kind, frame) != nil {
			return
		}
		// The frame is paid for after it is sent, so a paused socket holds none.
		if spent.WaitN(ctx, len(frame)) != nil || s.rpc.socketBytes.WaitN(ctx, len(frame)) != nil {
			return
		}
		awaitSocketPeer(s.upstream)
	}
}

func (s *solanaSocket) ping(ctx context.Context) {
	ticker := time.NewTicker(solanaSocketPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rpc.closing:
			closeSolanaSocket(s.client, websocket.CloseGoingAway, "restarting")
			return
		case <-ticker.C:
		}
		deadline := time.Now().Add(solanaSocketWriteWait)
		if s.client.WriteControl(websocket.PingMessage, nil, deadline) != nil || s.upstream.WriteControl(websocket.PingMessage, nil, deadline) != nil {
			return
		}
	}
}

func (s *solanaSocket) writeClient(kind int, body []byte) error {
	s.clientWrites.Lock()
	defer s.clientWrites.Unlock()
	_ = s.client.SetWriteDeadline(time.Now().Add(solanaSocketWriteWait))
	return s.client.WriteMessage(kind, body)
}

func closeSolanaSocket(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

// admit answers the bytes to send upstream or the refusal to send the client.
// Neither means the frame is a keepalive, which is dropped.
func (s *solanaSocket) admit(kind int, frame []byte) ([]byte, *solanaRPCRefusal) {
	refuse := func(id json.RawMessage, code int, message string) ([]byte, *solanaRPCRefusal) {
		refusal := refuseSolanaCall(id, code, message)
		return nil, &refusal
	}
	if kind != websocket.TextMessage {
		return refuse(nil, jsonRPCInvalidRequest, "text frames only")
	}
	call, ok := parseSolanaCall(frame)
	if !ok {
		return refuse(call.ID, jsonRPCInvalidRequest, "invalid request")
	}
	method := solanaMethods[call.Method]
	if method.class == solanaKeepalive {
		return nil, nil
	}
	if method.class != solanaSubscribe && method.class != solanaUnsubscribe {
		logSolanaRefusal("socket", "method", call.Method)
		return refuse(call.ID, jsonRPCMethodNotFound, "method not allowed: "+clipSolanaLog(call.Method))
	}
	params, ok := parseSolanaParams(call.Params)
	if !ok {
		return refuse(call.ID, jsonRPCInvalidParams, "invalid params")
	}
	if method.scope != nil && !method.scope(params) {
		logSolanaRefusal("socket", "unscoped", call.Method)
		return refuse(call.ID, jsonRPCInvalidParams, "params not allowed: "+call.Method)
	}
	id, ok := canonicalSolanaID(call.ID)
	if !ok {
		return refuse(call.ID, jsonRPCInvalidRequest, "an integer or short string id is required")
	}
	call.ID = id
	var reservation solanaReservation
	if method.class == solanaSubscribe {
		reservation = s.subscriptions.reserve(string(id))
	} else {
		if len(params) != 1 {
			return refuse(call.ID, jsonRPCInvalidParams, "invalid params")
		}
		target, err := json.Marshal(params[0])
		if err != nil {
			return refuse(call.ID, jsonRPCInvalidParams, "invalid params")
		}
		reservation = s.subscriptions.reserveUnsubscribe(string(id), string(target))
	}
	switch reservation {
	case solanaReservationDuplicate:
		return refuse(call.ID, jsonRPCInvalidRequest, "id is already in flight")
	case solanaReservationFull:
		return refuse(call.ID, jsonRPCServerError, "too many subscriptions on this socket")
	}
	// Re-encoded params collapse duplicate keys, so upstream parses what scope checked.
	encoded, err := json.Marshal(params)
	if err != nil {
		return refuse(call.ID, jsonRPCInvalidParams, "invalid params")
	}
	call.Params = encoded
	payload, err := json.Marshal(call)
	if err != nil {
		return refuse(call.ID, jsonRPCInvalidRequest, "invalid request")
	}
	return payload, nil
}

func parseSolanaParams(raw json.RawMessage) ([]any, bool) {
	if len(raw) == 0 {
		return []any{}, true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var params []any
	if decoder.Decode(&params) != nil {
		return nil, false
	}
	return params, true
}

var solanaIDForm = regexp.MustCompile(`^(?:[0-9]{1,18}|"[A-Za-z0-9_:-]{1,64}")$`)

// canonicalSolanaID admits only the forms upstream echoes byte for byte: "\u0061"
// and "a" would be two ids here and one upstream, and one's reply settles the
// other's reservation.
func canonicalSolanaID(id json.RawMessage) (json.RawMessage, bool) {
	return id, solanaIDForm.Match(id)
}

type solanaReservation int

const (
	solanaReserved solanaReservation = iota
	solanaReservationDuplicate
	solanaReservationFull
)

// solanaSubscriptions counts what a socket may hold upstream. A slot is taken
// when a subscribe goes up and freed only on upstream's word: an error reply, a
// true unsubscribe reply, or the final notification of a signature subscription,
// which upstream disposes of by itself. Any frame it cannot read leaves the slot
// taken.
type solanaSubscriptions struct {
	mu            sync.Mutex
	subscribing   map[string]struct{}
	unsubscribing map[string]string
	live          map[string]struct{}
}

func newSolanaSubscriptions() *solanaSubscriptions {
	return &solanaSubscriptions{subscribing: map[string]struct{}{}, unsubscribing: map[string]string{}, live: map[string]struct{}{}}
}

func (t *solanaSubscriptions) inFlight(id string) bool {
	_, subscribing := t.subscribing[id]
	_, unsubscribing := t.unsubscribing[id]
	return subscribing || unsubscribing
}

func (t *solanaSubscriptions) reserve(id string) solanaReservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight(id) {
		return solanaReservationDuplicate
	}
	if len(t.subscribing)+len(t.live) >= solanaSubscriptionsPerSocket {
		return solanaReservationFull
	}
	t.subscribing[id] = struct{}{}
	return solanaReserved
}

func (t *solanaSubscriptions) reserveUnsubscribe(id, target string) solanaReservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight(id) {
		return solanaReservationDuplicate
	}
	if len(t.unsubscribing) >= solanaSubscriptionsPerSocket {
		return solanaReservationFull
	}
	t.unsubscribing[id] = target
	return solanaReserved
}

type solanaUpstreamFrame struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Method string          `json:"method"`
	Params struct {
		Subscription json.RawMessage `json:"subscription"`
		Result       struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"params"`
}

func (t *solanaSubscriptions) observe(frame []byte) {
	if len(frame) > solanaSubscribeReplyBytes {
		return
	}
	var seen solanaUpstreamFrame
	if json.Unmarshal(frame, &seen) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if seen.Method == "signatureNotification" {
		// "receivedSignature" is an early notice; only the final {"err":...} ends it.
		if bytes.HasPrefix(seen.Params.Result.Value, []byte("{")) {
			delete(t.live, string(seen.Params.Subscription))
		}
		return
	}
	id := string(seen.ID)
	if _, subscribing := t.subscribing[id]; subscribing {
		if isSolanaSubscriptionID(seen.Result) {
			delete(t.subscribing, id)
			t.live[string(seen.Result)] = struct{}{}
		} else if len(seen.Error) > 0 && string(seen.Error) != "null" {
			delete(t.subscribing, id)
		}
		return
	}
	if target, unsubscribing := t.unsubscribing[id]; unsubscribing {
		delete(t.unsubscribing, id)
		if string(seen.Result) == "true" {
			delete(t.live, target)
		}
	}
}

func isSolanaSubscriptionID(result json.RawMessage) bool {
	if len(result) == 0 {
		return false
	}
	for _, digit := range result {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
