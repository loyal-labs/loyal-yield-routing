package webapp

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
)

var securityPolicy = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://challenges.cloudflare.com https://static.ads-twitter.com https://vercel.live",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data: blob: https:",
	"font-src 'self' data:",
	"object-src 'none'",
	"base-uri 'self'",
	"form-action 'self'",
	"child-src https://auth.privy.io https://verify.walletconnect.com https://verify.walletconnect.org",
	"frame-src https://auth.privy.io https://verify.walletconnect.com https://verify.walletconnect.org https://challenges.cloudflare.com https://vercel.live",
	"connect-src 'self' https://auth.privy.io https://api.privy.io https://*.rpc.privy.systems wss://relay.walletconnect.com wss://relay.walletconnect.org wss://www.walletlink.org https://explorer-api.walletconnect.com https://*.helius-rpc.com wss://*.helius-rpc.com https://api.mainnet-beta.solana.com https://api.devnet.solana.com https://api.jup.ag https://lite-api.jup.ag https://realtime.askloyal.com https://api.mixpanel.com https://stats.askloyal.com https://analytics.twitter.com https://vercel.live wss://vercel.live",
	"worker-src 'self' blob:",
	"manifest-src 'self'",
	"report-uri /api/csp-report",
}, "; ")

// Cloudflared reaches this loopback listener with the original client scheme.
func redirectHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, _ := net.SplitHostPort(r.RemoteAddr)
		if r.TLS == nil && net.ParseIP(peer).IsLoopback() && r.Header.Get("X-Forwarded-Proto") == "http" {
			target := *r.URL
			target.Scheme = "https"
			target.Host = r.Host
			http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// headers sets the security headers when the wrapped handler first writes its
// status, Set never Add: proxied /api and /ingest responses already carry
// Vercel's copies of these headers, and a header appearing twice is a browser
// error.
func headers(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secure := &securityResponse{
				ResponseWriter: w,
				release:        cfg.Release,
				path:           r.URL.Path,
				versioned:      r.URL.Query().Has("v"),
			}
			next.ServeHTTP(secure, r)
			secure.finish()
		})
	}
}

type securityResponse struct {
	http.ResponseWriter
	release   string
	path      string
	versioned bool
	applied   bool
}

func (s *securityResponse) WriteHeader(status int) {
	// An informational response (103 Early Hints from the upstream) is not
	// the response: net/http clears the header map after sending it, so the
	// headers go on the final status.
	if status >= 200 {
		s.apply()
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *securityResponse) Write(body []byte) (int, error) {
	s.apply()
	return s.ResponseWriter.Write(body)
}

func (s *securityResponse) finish() { s.apply() }

func (s *securityResponse) apply() {
	if s.applied {
		return
	}
	s.applied = true
	h := s.Header()
	h.Set("X-Loyal-Release", s.release)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Content-Security-Policy-Report-Only", securityPolicy)
	switch {
	case s.path == "/app/cherry" || s.path == "/app/cherry/":
		h.Set("Content-Security-Policy", "frame-ancestors 'self' https://chat.cherry.fun")
	default:
		h.Set("X-Frame-Options", "SAMEORIGIN")
	}
	if s.path == "/.well-known/apple-app-site-association" {
		h.Set("Content-Type", "application/json")
	}
	if s.versioned && strings.HasPrefix(s.path, "/wallet-workspace/facelift/earn-banner-") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

func (s *securityResponse) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *securityResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("webapp: ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}
