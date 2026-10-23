package webapp

// POST /api/observability/errors                                   <- web/apps/web/src/app/api/observability/errors/route.ts + mobile/errors/route.ts
// POST /api/observability/events                                   <- web/apps/web/src/app/api/observability/events/route.ts + mobile/events/route.ts
// POST /api/observability/metrics                                  <- web/apps/web/src/app/api/observability/metrics/route.ts + mobile/metrics/route.ts
// One Go op per signal serves both clients: a request is admitted through the
// browser same-origin check or the native gate (no Origin, no Sec-Fetch-Site)
// and parsed with the matching envelope, then forwarded to
// OBSERVABILITY_OTLP_ENDPOINT + /v1/{logs,traces,metrics}. Identity for the
// journey user id is the Principal, never a walletAddress body field.

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

var earnObservabilityFamily = earnFamily{
	name:     "observability ingest",
	ready:    func(h earnRoutes) bool { return h.OTLP != nil },
	register: earnRoutes.registerObservability,
}

const (
	obsErrorsPath  = "/api/observability/errors"
	obsEventsPath  = "/api/observability/events"
	obsMetricsPath = "/api/observability/metrics"
)

// obsIngestAccepted is Next's {accepted:true} 202 body.
type obsIngestAccepted struct {
	Accepted bool `json:"accepted"`
}

// obsJourneyAccepted adds the web events route's trafficKind; native admits
// and Next's mobile route omit it.
type obsJourneyAccepted struct {
	Accepted    bool   `json:"accepted"`
	TrafficKind string `json:"trafficKind,omitempty"`
}

// obsIngestRejected is Next's {error:"..."} failure body with its status; the
// header keeps every response no-store like jsonResponse does.
type obsIngestRejected struct {
	Message string `json:"error"`
	status  int
}

func (e *obsIngestRejected) Error() string  { return e.Message }
func (e *obsIngestRejected) GetStatus() int { return e.status }
func (e *obsIngestRejected) GetHeaders() http.Header {
	return http.Header{"Cache-Control": []string{"no-store"}}
}

// obsJourneyUnavailable is the events route's 503 telemetry_unavailable body.
type obsJourneyUnavailable struct {
	Message     string `json:"error"`
	TrafficKind string `json:"trafficKind"`
	status      int
}

func (e *obsJourneyUnavailable) Error() string  { return e.Message }
func (e *obsJourneyUnavailable) GetStatus() int { return e.status }
func (e *obsJourneyUnavailable) GetHeaders() http.Header {
	return http.Header{"Cache-Control": []string{"no-store"}}
}

var (
	obsErrForbidden       = &obsIngestRejected{Message: "forbidden", status: http.StatusForbidden}
	obsErrSyntheticTarget = &obsIngestRejected{Message: "synthetic_target_required", status: http.StatusForbidden}
	obsErrContentType     = &obsIngestRejected{Message: "invalid_content_type", status: http.StatusUnsupportedMediaType}
	obsErrRateLimited     = &obsIngestRejected{Message: "rate_limited", status: http.StatusTooManyRequests}
	obsErrTooLarge        = &obsIngestRejected{Message: "payload_too_large", status: http.StatusRequestEntityTooLarge}
	obsErrInvalidRequest  = &obsIngestRejected{Message: "invalid_request", status: http.StatusBadRequest}
)

// obsIngestInput carries everything the Next routes read off the Request.
type obsIngestInput struct {
	SessionInput
	Raw                 rawJSON
	Origin              string `header:"Origin"`
	SecFetchSite        string `header:"Sec-Fetch-Site"`
	ContentType         string `header:"Content-Type"`
	ContentLength       string `header:"Content-Length"`
	ForwardedProto      string `header:"X-Forwarded-Proto"`
	XForwardedFor       string `header:"X-Forwarded-For"`
	XVercelForwardedFor string `header:"X-Vercel-Forwarded-For"`
	CFConnectingIP      string `header:"CF-Connecting-IP"`
	XRealIP             string `header:"X-Real-IP"`
	SyntheticProbe      string `header:"X-Loyal-Synthetic-Probe"`
}

// obsHeader gives the rate limiter the same lookup Next does by header name.
func (in *obsIngestInput) obsHeader(name string) string {
	switch name {
	case "X-Forwarded-For":
		return in.XForwardedFor
	case "X-Vercel-Forwarded-For":
		return in.XVercelForwardedFor
	case "CF-Connecting-IP":
		return in.CFConnectingIP
	case "X-Real-IP":
		return in.XRealIP
	}
	return ""
}

type obsIngestOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	Body         obsIngestAccepted
}

type obsJourneyOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	Body         obsJourneyAccepted
}

func obsAcceptedOutput() *obsIngestOutput {
	return &obsIngestOutput{Status: http.StatusAccepted, CacheControl: "no-store", Body: obsIngestAccepted{Accepted: true}}
}

// obsReject maps the shared body-read errors onto their Next statuses.
func obsReject(err error) error {
	if err == errObservabilityTooLarge {
		return obsErrTooLarge
	}
	return obsErrInvalidRequest
}

// obsAdmit runs the shared admission checks: the native gate or the browser
// same-origin check, then the JSON content type, then the per-signal rate
// limit. The returned requestOrigin is what Next computes from request.url.
func obsAdmit(in *obsIngestInput, limiter *obsRateLimiter) (requestOrigin string, refused error) {
	requestOrigin = obsRequestOrigin(in.host, in.ForwardedProto)
	if !obsNativeAppRequest(in.Origin, in.SecFetchSite) &&
		!obsSameOriginRequest(in.Origin, in.SecFetchSite, requestOrigin) {
		return requestOrigin, obsErrForbidden
	}
	if !obsJSONContentType(in.ContentType) {
		return requestOrigin, obsErrContentType
	}
	if !obsConsumeRateLimit(limiter, in.obsHeader) {
		return requestOrigin, obsErrRateLimited
	}
	return requestOrigin, nil
}

// obsWallet is resolveWalletAddress (events/route.ts:70-79): the Principal's
// wallet when a session resolves, undefined otherwise, never an error.
func (h earnRoutes) obsWallet(ctx context.Context, in SessionInput) string {
	if h.auth == nil {
		return ""
	}
	session, err := h.auth.Session(in)
	if err != nil {
		return ""
	}
	return session.Wallet.String()
}

// The export helpers take the normalized events by pointer, matching the
// committed payload builders.
func browserErrorEventPtr(envelope obsErrorEnvelope, deploymentEnvironment, serverRelease string) *obsNormalizedError {
	event := createNormalizedBrowserErrorEvent(envelope, deploymentEnvironment, serverRelease)
	return &event
}

func mobileErrorEventPtr(envelope obsErrorEnvelope) *obsNormalizedError {
	event := normalizedMobileErrorEvent(envelope)
	return &event
}

func lifecycleEventPtr(event obsNormalizedLifecycle) *obsNormalizedLifecycle { return &event }

func metricEventPtr(event obsNormalizedMetric) *obsNormalizedMetric { return &event }

func (h earnRoutes) registerObservability(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	accepted := schemas.Schema(reflect.TypeOf(obsIngestAccepted{}), true, "")
	journey := schemas.Schema(reflect.TypeOf(obsJourneyAccepted{}), true, "")
	rejected := schemas.Schema(reflect.TypeOf(obsIngestRejected{}), true, "")
	unavailable := schemas.Schema(reflect.TypeOf(obsJourneyUnavailable{}), true, "")
	statuses := func(responses map[string]*huma.Response, schema *huma.Schema, codes ...int) {
		for _, code := range codes {
			responses[strconv.Itoa(code)] = &huma.Response{
				Description: http.StatusText(code),
				Content:     map[string]*huma.MediaType{"application/json": {Schema: schema}},
			}
		}
	}
	requestBody := &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/json": {Schema: &huma.Schema{Type: "object"}},
	}}

	errorsResponses := map[string]*huma.Response{}
	statuses(errorsResponses, rejected, 400, 403, 413, 415, 429)
	statuses(errorsResponses, accepted, 202)
	huma.Register(api, huma.Operation{
		OperationID: "post-observability-errors",
		Method:      http.MethodPost,
		Path:        obsErrorsPath,
		Summary:     "Ingest a browser or mobile error report and forward it to OTLP logs",
		Responses:   errorsResponses,
	}, h.obsErrors)
	api.OpenAPI().Paths[obsErrorsPath].Post.RequestBody = requestBody

	eventsResponses := map[string]*huma.Response{}
	statuses(eventsResponses, journey, 202)
	statuses(eventsResponses, rejected, 400, 403, 413, 415, 429)
	statuses(eventsResponses, unavailable, 503)
	huma.Register(api, huma.Operation{
		OperationID: "post-observability-events",
		Method:      http.MethodPost,
		Path:        obsEventsPath,
		Summary:     "Ingest a flow lifecycle event and forward it to OTLP logs and traces",
		Responses:   eventsResponses,
	}, h.obsEvents)
	api.OpenAPI().Paths[obsEventsPath].Post.RequestBody = requestBody

	metricsResponses := map[string]*huma.Response{}
	statuses(metricsResponses, rejected, 400, 403, 413, 415, 429)
	statuses(metricsResponses, accepted, 202)
	huma.Register(api, huma.Operation{
		OperationID: "post-observability-metrics",
		Method:      http.MethodPost,
		Path:        obsMetricsPath,
		Summary:     "Ingest a loading metric and forward it to OTLP metrics",
		Responses:   metricsResponses,
	}, h.obsMetrics)
	api.OpenAPI().Paths[obsMetricsPath].Post.RequestBody = requestBody
}

// obsErrors ports the two error routes. Web rejects third-party extension and
// Cap widget reports without exporting; every other outcome is 202 whether or
// not the collector answered, exactly like Next.
func (h earnRoutes) obsErrors(ctx context.Context, in *obsIngestInput) (*obsIngestOutput, error) {
	requestOrigin, refused := obsAdmit(in, obsErrorRates)
	if refused != nil {
		return nil, refused
	}
	raw, err := obsReadBody(in.ContentLength, in.Raw.data)
	if err != nil {
		return nil, obsReject(err)
	}
	now := time.Now()
	if obsNativeAppRequest(in.Origin, in.SecFetchSite) {
		envelope, parseErr := parseMobileErrorEnvelope(raw, now)
		if parseErr != nil {
			return nil, obsReject(parseErr)
		}
		h.exportObsErrorEvent(ctx, mobileErrorEventPtr(envelope))
		return obsAcceptedOutput(), nil
	}
	envelope, parseErr := parseBrowserErrorEnvelope(raw, requestOrigin, now)
	if parseErr != nil {
		return nil, obsReject(parseErr)
	}
	if isThirdPartyExtensionError(envelope.operation, envelope.stack) ||
		isCapWidgetInternalError(envelope.operation, envelope.message) {
		return obsAcceptedOutput(), nil
	}
	h.exportObsErrorEvent(ctx, browserErrorEventPtr(envelope, observabilityDeploymentEnvironment(), observabilityRelease()))
	return obsAcceptedOutput(), nil
}

// obsEvents ports the two lifecycle routes. Only the web admit reports the
// traffic kind and depends on the export succeeding; the mobile admit always
// answers 202 like Next's mobile route.
func (h earnRoutes) obsEvents(ctx context.Context, in *obsIngestInput) (*obsJourneyOutput, error) {
	if in.SyntheticProbe == "1" && journeyTrafficKind("") != "synthetic" {
		return nil, obsErrSyntheticTarget
	}
	_, refused := obsAdmit(in, obsLifecycleRates)
	if refused != nil {
		return nil, refused
	}
	raw, err := obsReadBody(in.ContentLength, in.Raw.data)
	if err != nil {
		return nil, obsReject(err)
	}
	now := time.Now()
	if obsNativeAppRequest(in.Origin, in.SecFetchSite) {
		envelope, parseErr := parseMobileLifecycleEnvelope(raw, now)
		if parseErr != nil {
			return nil, obsReject(parseErr)
		}
		h.exportObsLifecycleEvent(ctx, lifecycleEventPtr(obsMobileLifecycleEvent(envelope)))
		return &obsJourneyOutput{Status: http.StatusAccepted, CacheControl: "no-store", Body: obsJourneyAccepted{Accepted: true}}, nil
	}
	envelope, parseErr := parseBrowserLifecycleEnvelope(raw, now)
	if parseErr != nil {
		return nil, obsReject(parseErr)
	}
	wallet := h.obsWallet(ctx, in.SessionInput)
	trafficKind := journeyTrafficKind(wallet)
	exported := h.exportObsLifecycleEvent(ctx, lifecycleEventPtr(obsWebLifecycleEvent(envelope, wallet)))
	if !exported {
		return nil, &obsJourneyUnavailable{Message: "telemetry_unavailable", TrafficKind: trafficKind, status: http.StatusServiceUnavailable}
	}
	return &obsJourneyOutput{Status: http.StatusAccepted, CacheControl: "no-store", Body: obsJourneyAccepted{Accepted: true, TrafficKind: trafficKind}}, nil
}

// obsMetrics ports the two loading-metric routes; like Next's metrics routes
// the export result never changes the 202.
func (h earnRoutes) obsMetrics(ctx context.Context, in *obsIngestInput) (*obsIngestOutput, error) {
	_, refused := obsAdmit(in, obsMetricRates)
	if refused != nil {
		return nil, refused
	}
	raw, err := obsReadBody(in.ContentLength, in.Raw.data)
	if err != nil {
		return nil, obsReject(err)
	}
	now := time.Now()
	if obsNativeAppRequest(in.Origin, in.SecFetchSite) {
		envelope, parseErr := parseMobileLoadingMetricEnvelope(raw, now)
		if parseErr != nil {
			return nil, obsReject(parseErr)
		}
		h.exportObsLoadingMetric(ctx, metricEventPtr(obsMobileLoadingMetricEvent(envelope)))
		return obsAcceptedOutput(), nil
	}
	envelope, parseErr := parseBrowserLoadingMetricEnvelope(raw, now)
	if parseErr != nil {
		return nil, obsReject(parseErr)
	}
	h.exportObsLoadingMetric(ctx, metricEventPtr(obsWebLoadingMetricEvent(envelope)))
	return obsAcceptedOutput(), nil
}
