package webapp

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const mobileEarnPrefix = "/api/smart-accounts/mobile/"

type budget struct {
	every time.Duration
	burst int
}

type routeClass struct {
	name           string
	wallet, client budget
}

var (
	mobileEarnSigning = routeClass{
		name:   "signing",
		wallet: budget{every: 5 * time.Second, burst: 12},
		client: budget{every: 2 * time.Second, burst: 30},
	}
	mobileEarnReads = routeClass{
		name:   "reads",
		wallet: budget{every: time.Second / 3, burst: 120},
		client: budget{every: time.Second / 10, burst: 300},
	}
)

const (
	limiterIdleTTL      = 10 * time.Minute
	limiterSweepEvery   = time.Minute
	walletPeekBodyBytes = 64 << 10
)

func classOf(method string) routeClass {
	if method == http.MethodGet || method == http.MethodHead {
		return mobileEarnReads
	}
	return mobileEarnSigning
}

type limiterKey struct {
	class, kind, id string
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type limiters struct {
	mu        sync.Mutex
	entries   map[limiterKey]*limiterEntry
	lastSweep time.Time
}

func newLimiters() *limiters {
	return &limiters{entries: map[limiterKey]*limiterEntry{}}
}

func (l *limiters) get(key limiterKey, b budget, now time.Time) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) >= limiterSweepEvery {
		for k, e := range l.entries {
			if now.Sub(e.lastSeen) >= limiterIdleTTL {
				delete(l.entries, k)
			}
		}
		l.lastSweep = now
	}
	e, ok := l.entries[key]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(rate.Every(b.every), b.burst)}
		l.entries[key] = e
	}
	e.lastSeen = now
	return e.limiter
}

func limitMobileEarn(next http.Handler) http.Handler {
	buckets := newLimiters()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		class := classOf(r.Method)
		client := buckets.get(limiterKey{class.name, "client", clientIP(r)}, class.client, now)
		if !client.AllowN(now, 1) {
			tooMany(w, client, class.client, now)
			return
		}
		walletAddress := walletOf(r)
		if walletAddress == "" {
			next.ServeHTTP(w, r)
			return
		}
		wallet := buckets.get(limiterKey{class.name, "wallet", walletAddress}, class.wallet, now)
		if wallet.TokensAt(now) < 1 {
			tooMany(w, wallet, class.wallet, now)
			return
		}
		status := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(status, r)
		if status.code != http.StatusUnauthorized {
			wallet.ReserveN(time.Now(), 1)
		}
	})
}

func tooMany(w http.ResponseWriter, limiter *rate.Limiter, b budget, now time.Time) {
	wait := time.Duration((1 - limiter.TokensAt(now)) * float64(b.every))
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
	http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
}

func clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if forwarded := r.Header.Get("CF-Connecting-IP"); forwarded != "" && net.ParseIP(peer).IsLoopback() {
		return forwarded
	}
	return peer
}

func walletOf(r *http.Request) string {
	if wallet := r.URL.Query().Get("walletAddress"); wallet != "" {
		return wallet
	}
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	peek, err := io.ReadAll(io.LimitReader(r.Body, walletPeekBodyBytes))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
	if err != nil {
		return ""
	}
	var fields struct {
		WalletAddress string `json:"walletAddress"`
	}
	if json.Unmarshal(peek, &fields) != nil {
		return ""
	}
	return fields.WalletAddress
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 && code >= 200 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(body []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(body)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
