package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// One wallet tab opens 3 sockets, and past the cap a tab cannot confirm
	// its sends, so this allows 8 tabs behind one address.
	solanaSocketsPerClient       = 24
	solanaSubscriptionsPerSocket = 32
	solanaSocketFrameBytes       = 16 << 10
	// A subscribe reply is a short id and integer, so a longer upstream frame
	// is a notification and is never decoded.
	solanaSubscribeReplyBytes = 1 << 10
	solanaSocketDialWait      = 10 * time.Second
	solanaSocketWriteWait     = 10 * time.Second
	solanaSocketPingEvery     = 30 * time.Second
	solanaSocketReadWait      = 75 * time.Second
)

var (
	solanaSocketUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	solanaSocketDialer   = websocket.Dialer{HandshakeTimeout: solanaSocketDialWait}
)

func (s *SolanaRPC) serveSocket(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
		return
	}
	ip := clientKey(r)
	if !s.spend(w, solanaReads, ip, 1) {
		return
	}
	if origin := r.Header.Get("Origin"); !s.allowedSocketOrigin(origin) {
		logSolanaRefusal("socket", "origin", origin)
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if !s.acquireSocket(ip) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	defer s.releaseSocket(ip)

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
	client.SetReadLimit(solanaSocketFrameBytes)
	socket := &solanaSocket{rpc: s, ip: ip, client: client, upstream: upstream, subscriptions: newSolanaSubscriptions()}
	socket.run()
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
	if s.sockets[ip] >= solanaSocketsPerClient {
		return false
	}
	s.sockets[ip]++
	return true
}

func (s *SolanaRPC) releaseSocket(ip string) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()
	if s.sockets[ip]--; s.sockets[ip] <= 0 {
		delete(s.sockets, ip)
	}
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
	stop := make(chan struct{})
	end := sync.OnceFunc(func() {
		close(stop)
		_ = s.client.Close()
		_ = s.upstream.Close()
	})
	var relays sync.WaitGroup
	relays.Go(func() {
		defer end()
		s.relayUpstream()
	})
	relays.Go(func() {
		defer end()
		s.ping(stop)
	})
	s.relayClient()
	end()
	relays.Wait()
}

func awaitSocketPeer(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(solanaSocketReadWait))
}

func (s *solanaSocket) relayClient() {
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
func (s *solanaSocket) relayUpstream() {
	defer closeSolanaSocket(s.client, websocket.CloseGoingAway, "upstream closed")
	awaitSocketPeer(s.upstream)
	s.upstream.SetPongHandler(func(string) error {
		awaitSocketPeer(s.upstream)
		return nil
	})
	for {
		kind, frame, err := s.upstream.ReadMessage()
		if err != nil {
			return
		}
		awaitSocketPeer(s.upstream)
		s.subscriptions.observe(frame)
		if s.writeClient(kind, frame) != nil {
			return
		}
	}
}

func (s *solanaSocket) ping(stop <-chan struct{}) {
	ticker := time.NewTicker(solanaSocketPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
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
	now := time.Now()
	if !s.rpc.buckets.get(limiterKey{solanaReads.name, "client", s.ip}, solanaReads.client, now).AllowN(now, 1) {
		return refuse(call.ID, jsonRPCServerError, "rate limited")
	}
	if method.class != solanaSubscribe && method.class != solanaUnsubscribe {
		logSolanaRefusal("socket", "method", call.Method)
		return refuse(call.ID, jsonRPCMethodNotFound, "method not allowed: "+clipSolanaLog(call.Method))
	}
	params, ok := parseSolanaSocketParams(call.Params)
	if !ok {
		return refuse(call.ID, jsonRPCInvalidParams, "invalid params")
	}
	if method.scope != nil && !method.scope(params) {
		logSolanaRefusal("socket", "unscoped", call.Method)
		return refuse(call.ID, jsonRPCInvalidParams, "params not allowed: "+call.Method)
	}
	if !solanaScalarID(call.ID) {
		return refuse(call.ID, jsonRPCInvalidRequest, "a scalar id is required")
	}
	id := string(call.ID)
	if method.class == solanaSubscribe {
		switch s.subscriptions.reserve(id) {
		case solanaReservationDuplicate:
			return refuse(call.ID, jsonRPCInvalidRequest, "id is already in flight")
		case solanaReservationFull:
			return refuse(call.ID, jsonRPCServerError, "too many subscriptions on this socket")
		}
	} else if !s.subscriptions.release(id, params) {
		return refuse(call.ID, jsonRPCInvalidRequest, "id is already in flight")
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

func parseSolanaSocketParams(raw json.RawMessage) ([]any, bool) {
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

func solanaScalarID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var scalar any
	if json.Unmarshal(id, &scalar) != nil {
		return false
	}
	switch scalar.(type) {
	case string, float64:
		return true
	}
	return false
}

type solanaReservation int

const (
	solanaReserved solanaReservation = iota
	solanaReservationDuplicate
	solanaReservationFull
)

// solanaSubscriptions counts what a socket holds upstream. Only ids upstream
// handed out free a slot: counting unsubscribe frames instead would let a
// client free slots with made-up ids and exceed the cap.
type solanaSubscriptions struct {
	mu      sync.Mutex
	pending map[string]struct{}
	live    map[string]struct{}
}

func newSolanaSubscriptions() *solanaSubscriptions {
	return &solanaSubscriptions{pending: map[string]struct{}{}, live: map[string]struct{}{}}
}

func (t *solanaSubscriptions) reserve(id string) solanaReservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, inFlight := t.pending[id]; inFlight {
		return solanaReservationDuplicate
	}
	if len(t.pending)+len(t.live) >= solanaSubscriptionsPerSocket {
		return solanaReservationFull
	}
	t.pending[id] = struct{}{}
	return solanaReserved
}

// release refuses an unsubscribe that reuses an in-flight subscribe id: its
// reply would settle that subscribe and leave the real subscription uncounted.
func (t *solanaSubscriptions) release(id string, params []any) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, inFlight := t.pending[id]; inFlight {
		return false
	}
	if len(params) == 1 {
		if subscription, err := json.Marshal(params[0]); err == nil {
			delete(t.live, string(subscription))
		}
	}
	return true
}

func (t *solanaSubscriptions) observe(frame []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 || len(frame) > solanaSubscribeReplyBytes {
		return
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(frame, &reply) != nil {
		return
	}
	id := string(reply["id"])
	if _, inFlight := t.pending[id]; !inFlight {
		return
	}
	delete(t.pending, id)
	if result, ok := reply["result"]; ok {
		t.live[string(result)] = struct{}{}
	}
}
