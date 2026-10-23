package webapp

// The observability ingest server half, porting the config, enrichment and
// transport of web/apps/web/src/features/observability/server.ts and the
// per-route admission shared by the six Next routes under
// web/apps/web/src/app/api/observability/. Go serves the web path once: a
// request passes either the browser same-origin check or the native-app gate
// (native-gate.server.ts:5-9) and is parsed with the matching envelope.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// parseOTLPConfig ports getTelemetryConfig (server.ts:51-74) minus the
// per-signal path rewrite the exporter applies. Both values must be present
// and the endpoint https (anywhere) or http on loopback.
func parseOTLPConfig(rawEndpoint, ingestionKey string) *OTLP {
	rawEndpoint = strings.TrimSpace(rawEndpoint)
	ingestionKey = strings.TrimSpace(ingestionKey)
	if rawEndpoint == "" || ingestionKey == "" {
		return nil
	}
	parsed, err := url.Parse(rawEndpoint)
	if err != nil || !obsURLOriginLocal(parsed) {
		return nil
	}
	return &OTLP{Endpoint: parsed, IngestionKey: ingestionKey}
}

// observabilityRelease ports getObservabilityRelease (server.ts:76).
func observabilityRelease() string {
	release := os.Getenv("VERCEL_GIT_COMMIT_SHA")
	if release == "" {
		release = os.Getenv("NEXT_PUBLIC_GIT_COMMIT_HASH")
	}
	if release == "" {
		release = "unknown"
	}
	if normalized := normalizeResourceValue(release, obsMaxReleaseLength); normalized != "" {
		return normalized
	}
	return "unknown"
}

// observabilityDeploymentEnvironment ports getObservabilityDeploymentEnvironment
// (server.ts:87).
func observabilityDeploymentEnvironment() string {
	environment := os.Getenv("VERCEL_ENV")
	if environment == "" {
		environment = os.Getenv("NEXT_PUBLIC_APP_ENVIRONMENT")
	}
	if environment == "" {
		environment = "unknown"
	}
	if normalized := normalizeResourceValue(environment, obsMaxEnvironmentLength); normalized != "" {
		return normalized
	}
	return "unknown"
}

// journeyTrafficKind ports getJourneyTrafficKind (server.ts:248): synthetic
// when the env flag says so outside production, internal for the allowlist,
// external otherwise.
func journeyTrafficKind(wallet string) string {
	environment := observabilityDeploymentEnvironment()
	if os.Getenv("OBSERVABILITY_TRAFFIC_KIND") == "synthetic" &&
		environment != "production" && environment != "prod" {
		return "synthetic"
	}
	if wallet != "" {
		allowlist := os.Getenv("OBSERVABILITY_INTERNAL_WALLETS")
		if allowlist == "" {
			allowlist = obsInternalWalletDefault
		}
		for _, value := range strings.Split(allowlist, ",") {
			if strings.TrimSpace(value) == wallet {
				return "internal"
			}
		}
	}
	return "external"
}

// obsNativeAppRequest ports isNativeAppRequest (native-gate.server.ts:5-9):
// native fetch never sends Origin or Sec-Fetch-Site, browsers on cross-site
// POSTs always do.
func obsNativeAppRequest(origin, secFetchSite string) bool {
	return origin == "" && secFetchSite == ""
}

// obsSameOriginRequest ports isSameOriginRequest (errors/route.ts:22-38): the
// Origin must parse to the request's own origin and Sec-Fetch-Site, when
// present, must be same-origin. requestOrigin is what new URL(request.url).origin
// is on Next: the scheme the proxy forwarded plus this request's host.
func obsSameOriginRequest(originHeader, secFetchSite, requestOrigin string) bool {
	if originHeader == "" {
		return false
	}
	origin, err := url.Parse(originHeader)
	if err != nil {
		return false
	}
	if obsURLOrigin(origin) != requestOrigin {
		return false
	}
	return secFetchSite == "" || secFetchSite == "same-origin"
}

// obsRequestOrigin builds new URL(request.url).origin from the pieces the
// proxy hands the Go server (headers.go's redirectHTTPS shows the scheme
// convention).
func obsRequestOrigin(host, forwardedProto string) string {
	scheme := "https"
	if forwardedProto == "http" {
		scheme = "http"
	}
	origin, err := url.Parse(scheme + "://" + host)
	if err != nil {
		return ""
	}
	return obsURLOrigin(origin)
}

// obsJSONContentType ports isJsonRequest: the media type before the first
// semicolon, trimmed, must be application/json.
func obsJSONContentType(contentType string) bool {
	return strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]) == "application/json"
}

// obsReadBody ports readJsonBody: declared and actual byte budgets, then a
// fatal UTF-8 decode before the JSON parse. The rawJSON resolver already
// capped the read at maxJSONBody, so anything larger than the contract limit
// arrives truncated and fails the byte check below.
func obsReadBody(declaredContentLength string, body []byte) ([]byte, error) {
	if declaredContentLength != "" {
		parsed, ok := obsPositiveInt(declaredContentLength)
		if !ok {
			return nil, errObservabilityEnvelope
		}
		if parsed > obsMaxRequestBytes {
			return nil, errObservabilityTooLarge
		}
	}
	if len(body) == 0 {
		return nil, errObservabilityEnvelope
	}
	if len(body) > obsMaxRequestBytes {
		return nil, errObservabilityTooLarge
	}
	// TextDecoder("utf-8", { fatal: true }) rejects invalid UTF-8 before the
	// JSON.parse; encoding/json would silently coerce to U+FFFD instead.
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errObservabilityEnvelope
	}
	return body, nil
}

// obsPositiveInt mirrors Number(declaredLength) plus the integer and
// non-negative guards: ASCII decimal digits only.
func obsPositiveInt(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return n, true
		}
	}
	return n, true
}

// obsWalletHash ports the SHA-256 hex userId the web events route derives
// (server.ts:276-285).
func obsWalletHash(wallet string) string {
	sum := sha256.Sum256([]byte(wallet))
	return hex.EncodeToString(sum[:])
}

// obsExportBudget is OBSERVABILITY_EXPORT_BUDGET_MS (budget-contract.ts).
const obsExportBudget = 1250 * time.Millisecond

// The ingest rate limits, porting rate-limit.server.ts: fixed 60s windows per
// source, one bucket per signal family, 1024 tracked sources with the oldest
// evicted. TS module maps keep insertion order, so the Go limiter carries its
// own queue instead of relying on Go map iteration order.
const (
	obsRateWindowMS      = 60 * 1000
	obsMaxTrackedSources = 1024
)

type obsRateEntry struct {
	count     int
	resetAtMS int64
}

type obsRateLimiter struct {
	mu      sync.Mutex
	entries map[string]*obsRateEntry
	order   []string
	max     int
}

func newObsRateLimiter(maxReports int) *obsRateLimiter {
	return &obsRateLimiter{entries: map[string]*obsRateEntry{}, max: maxReports}
}

func (l *obsRateLimiter) allow(key string, nowMS int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.order[:0]
	for _, existing := range l.order {
		entry, ok := l.entries[existing]
		if !ok {
			continue
		}
		if entry.resetAtMS <= nowMS {
			delete(l.entries, existing)
			continue
		}
		kept = append(kept, existing)
	}
	l.order = kept
	if current, ok := l.entries[key]; ok {
		if current.count >= l.max {
			return false
		}
		current.count++
		return true
	}
	if len(l.entries) >= obsMaxTrackedSources {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.entries, oldest)
	}
	l.entries[key] = &obsRateEntry{count: 1, resetAtMS: nowMS + obsRateWindowMS}
	l.order = append(l.order, key)
	return true
}

// The three limiter instances mirror the three module-level TS maps.
var (
	obsErrorRates     = newObsRateLimiter(20)
	obsLifecycleRates = newObsRateLimiter(120)
	obsMetricRates    = newObsRateLimiter(240)
)

func resetObsRateLimitsForTests() {
	obsErrorRates = newObsRateLimiter(20)
	obsLifecycleRates = newObsRateLimiter(120)
	obsMetricRates = newObsRateLimiter(240)
}

// obsRateLimitSource ports getRequestSource (rate-limit.server.ts:20-33): the
// first proxy header that carries an address, else "unknown", hashed so raw
// addresses never become map keys.
func obsRateLimitSource(header func(string) string) string {
	source := strings.TrimSpace(strings.SplitN(header("X-Forwarded-For"), ",", 2)[0])
	if source == "" {
		source = strings.TrimSpace(header("X-Vercel-Forwarded-For"))
	}
	if source == "" {
		source = strings.TrimSpace(header("CF-Connecting-IP"))
	}
	if source == "" {
		source = strings.TrimSpace(header("X-Real-IP"))
	}
	if source == "" {
		source = "unknown"
	}
	// slice(0, 128) counts UTF-16 units in TS; runes are the same count here
	// for every address these headers can hold.
	runes := []rune(source)
	if len(runes) > 128 {
		runes = runes[:128]
	}
	sum := sha256.Sum256([]byte(string(runes)))
	return hex.EncodeToString(sum[:])
}

func obsConsumeRateLimit(limiter *obsRateLimiter, header func(string) string) bool {
	return limiter.allow(obsRateLimitSource(header), time.Now().UnixMilli())
}

// obsEncodeJSON is JSON.stringify for the collector payload: no HTML
// escaping, no trailing newline. < and & inside an error message must reach
// the collector as literal bytes, not <.
func obsEncodeJSON(payload any) ([]byte, bool) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, false
	}
	return bytes.TrimRight(out.Bytes(), "\n"), true
}

// exportOtlpPayload ports exportOtlpPayload (server.ts:108-140): one POST of
// the JSON payload to <endpoint>/v1/<signal> inside the export budget, ok iff
// the collector answers 2xx. A missing or invalid config means the event is
// dropped, never an error.
func (h earnRoutes) exportOtlpPayload(ctx context.Context, payload any, signal string) bool {
	if h.OTLP == nil || h.OTLP.Endpoint == nil || h.OTLP.IngestionKey == "" {
		return false
	}
	endpoint := *h.OTLP.Endpoint
	endpoint.Path = "/v1/" + signal
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	endpoint.RawFragment = ""
	body, ok := obsEncodeJSON(payload)
	if !ok {
		return false
	}
	sendCtx, cancel := context.WithTimeout(ctx, obsExportBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", h.OTLP.IngestionKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := h.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (h earnRoutes) exportObsErrorEvent(ctx context.Context, event *obsNormalizedError) bool {
	return h.exportOtlpPayload(ctx, buildOtlpErrorPayload(event), "logs")
}

// exportObsLifecycleEvent ports exportLifecycleEvent (server.ts:146-155): the
// lifecycle log plus, with full trace context, the journey trace; both must
// land for the events route to answer 202.
func (h earnRoutes) exportObsLifecycleEvent(ctx context.Context, event *obsNormalizedLifecycle) bool {
	if !h.exportOtlpPayload(ctx, buildOtlpLifecyclePayload(event), "logs") {
		return false
	}
	if traces := buildOtlpJourneyTracePayload(event); traces != nil {
		return h.exportOtlpPayload(ctx, traces, "traces")
	}
	return true
}

func (h earnRoutes) exportObsLoadingMetric(ctx context.Context, event *obsNormalizedMetric) bool {
	return h.exportOtlpPayload(ctx, buildOtlpLoadingMetricPayload(event), "metrics")
}

// obsWebLifecycleEvent is reportBrowserLifecycleEnvelope's enrichment
// (server.ts:266-295): the Principal wallet, its hashed user id and the
// traffic kind, plus the server's own release/environment.
func obsWebLifecycleEvent(envelope obsNormalizedLifecycle, wallet string) obsNormalizedLifecycle {
	event := envelope
	event.walletAddress = wallet
	if wallet != "" {
		event.userID = obsWalletHash(wallet)
	}
	event.trafficKind = journeyTrafficKind(wallet)
	event.deploymentEnvironment = observabilityDeploymentEnvironment()
	event.release = observabilityRelease()
	event.serviceName = "loyal-frontend"
	return event
}

// obsMobileLifecycleEvent is reportMobileLifecycleEnvelope (server.ts:328):
// the envelope already carries environment/release/wallet, only the service
// name is added. No user id or traffic kind — Next never sets them here.
func obsMobileLifecycleEvent(envelope obsNormalizedLifecycle) obsNormalizedLifecycle {
	event := envelope
	event.serviceName = "loyal-mobile"
	return event
}

func obsWebLoadingMetricEvent(envelope obsNormalizedMetric) obsNormalizedMetric {
	event := envelope
	event.deploymentEnvironment = observabilityDeploymentEnvironment()
	event.release = observabilityRelease()
	event.serviceName = "loyal-frontend"
	return event
}

// obsMobileLoadingMetricEvent is reportMobileLoadingMetricEnvelope
// (server.ts:312): the envelope's environment/release win, the service name
// flips the metric description and scope.
func obsMobileLoadingMetricEvent(envelope obsNormalizedMetric) obsNormalizedMetric {
	event := envelope
	event.serviceName = "loyal-mobile"
	return event
}
