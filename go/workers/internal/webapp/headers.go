package webapp

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
)

// securityPolicy is apps/web next.config.ts:44-62's CONTENT_SECURITY_POLICY,
// directive for directive. Report-Only: violations POST to /api/csp-report and
// nothing is blocked.
var securityPolicy = strings.Join([]string{
	"default-src 'self'",
	// Turnstile, X pixel (uwt.js), Vercel Live feedback widget.
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://challenges.cloudflare.com https://static.ads-twitter.com https://vercel.live",
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self' data: blob: https:",
	"font-src 'self' data:",
	"object-src 'none'",
	"base-uri 'self'",
	"form-action 'self'",
	"child-src https://auth.privy.io https://verify.walletconnect.com https://verify.walletconnect.org",
	"frame-src https://auth.privy.io https://verify.walletconnect.com https://verify.walletconnect.org https://challenges.cloudflare.com https://vercel.live",
	// Own API + Privy + Solana RPC/WS (Helius) + Jupiter + realtime + analytics.
	"connect-src 'self' https://auth.privy.io https://api.privy.io https://*.rpc.privy.systems wss://relay.walletconnect.com wss://relay.walletconnect.org wss://www.walletlink.org https://explorer-api.walletconnect.com https://*.helius-rpc.com wss://*.helius-rpc.com https://api.mainnet-beta.solana.com https://api.devnet.solana.com https://api.jup.ag https://lite-api.jup.ag https://realtime.askloyal.com https://api.mixpanel.com https://stats.askloyal.com https://analytics.twitter.com https://vercel.live wss://vercel.live",
	"worker-src 'self' blob:",
	"manifest-src 'self'",
	"report-uri /api/csp-report",
}, "; ")

// headers sets what every response carries, equal to apps/web's
// next.config.ts:44-74,124-168: the common security headers everywhere,
// frame-ancestors instead of X-Frame-Options on the Cherry entry, a JSON
// content type for the Apple association, and immutable caching for the
// versioned Earn banner artwork.
//
// Headers are applied when the wrapped handler first writes its status, Set
// never Add: proxied /api and /ingest responses already carry Vercel's copies
// of these headers, and a header appearing twice is a browser error.
func headers(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Loyal-Release", cfg.Release)
			secure := &securityResponse{
				ResponseWriter: w,
				path:           r.URL.Path,
				versioned:      r.URL.Query().Has("v"),
			}
			next.ServeHTTP(secure, r)
			secure.finish()
		})
	}
}

// securityResponse applies the security headers just before the response
// goes out, over whatever the handler (or the proxied upstream) put in the
// header map.
type securityResponse struct {
	http.ResponseWriter
	path      string
	versioned bool
	applied   bool
}

func (s *securityResponse) WriteHeader(status int) {
	s.apply()
	s.ResponseWriter.WriteHeader(status)
}

func (s *securityResponse) Write(body []byte) (int, error) {
	s.apply()
	return s.ResponseWriter.Write(body)
}

// finish covers a handler that returned without writing anything; Set is
// idempotent, so double application is harmless.
func (s *securityResponse) finish() { s.apply() }

func (s *securityResponse) apply() {
	if s.applied {
		return
	}
	s.applied = true
	h := s.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Content-Security-Policy-Report-Only", securityPolicy)
	switch {
	case s.path == "/app/cherry" || s.path == "/app/cherry/":
		h.Set("Content-Security-Policy", "frame-ancestors 'self' https://chat.cherry.fun")
	default:
		h.Set("X-Frame-Options", "SAMEORIGIN")
	}
	// Apple's CDN requires application/json for Universal Links
	// (next.config.ts:127-131); the file has no extension to infer it from.
	if s.path == "/.well-known/apple-app-site-association" {
		h.Set("Content-Type", "application/json")
	}
	// The Earn banner carousel remounts its artwork every few seconds; the
	// ?v=<commit> buster makes immutable safe (next.config.ts:134-148).
	if s.versioned && strings.HasPrefix(s.path, "/wallet-workspace/facelift/earn-banner-") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

// Flush passes streaming writes through so an SSE response reaches the
// client as the upstream produces it.
func (s *securityResponse) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack lets upgraded connections (websockets) through untouched.
func (s *securityResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("webapp: ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}
